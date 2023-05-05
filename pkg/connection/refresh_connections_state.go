package connection

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/turbot/go-kit/helpers"
	"github.com/turbot/steampipe/pkg/constants"
	"github.com/turbot/steampipe/pkg/db/db_common"
	"github.com/turbot/steampipe/pkg/db/db_local"
	"github.com/turbot/steampipe/pkg/error_helpers"
	"github.com/turbot/steampipe/pkg/statushooks"
	"github.com/turbot/steampipe/pkg/steampipeconfig"
	"github.com/turbot/steampipe/pkg/utils"
	"github.com/turbot/steampipe/sperr"
	"golang.org/x/exp/maps"
	"golang.org/x/sync/semaphore"
	"log"
	"strings"
	"sync"
)

type refreshConnectionState struct {
	pool                       *pgxpool.Pool
	searchPath                 []string
	connectionUpdates          *steampipeconfig.ConnectionUpdates
	tableUpdater               *connectionStateTableUpdater
	res                        *steampipeconfig.RefreshConnectionResult
	forceUpdateConnectionNames []string
}

func newRefreshConnectionState(ctx context.Context, forceUpdateConnectionNames []string) (*refreshConnectionState, error) {
	// create a connection pool to connection refresh
	poolsize := 1
	pool, err := db_local.CreateConnectionPool(ctx, &db_local.CreateDbOptions{Username: constants.DatabaseSuperUser}, poolsize)
	if err != nil {
		return nil, err
	}

	// set user search path first
	log.Printf("[WARN] Setting up search path")
	searchPath, err := db_local.SetUserSearchPath(ctx, pool)
	if err != nil {
		// note: close pool in case of error
		pool.Close()
		return nil, err
	}

	return &refreshConnectionState{
		pool:                       pool,
		searchPath:                 searchPath,
		forceUpdateConnectionNames: forceUpdateConnectionNames,
	}, nil
}

func (state *refreshConnectionState) close() {
	if state.pool != nil {
		state.pool.Close()
	}
}

// RefreshConnections loads required connections from config
// and update the database schema and search path to reflect the required connections
// return whether any changes have been made
func (state *refreshConnectionState) refreshConnections(ctx context.Context) {
	utils.LogTime("db.refreshConnections start")
	defer utils.LogTime("db.refreshConnections end")

	defer func() {
		if state.res.Error != nil {
			// if there was an error (other than a connection error, which will NOT have been assigned to res),
			// set state of all connections to error
			// TODO KAI CHECK THIS
			state.setAllConnectionStateToError(ctx, state.res.Error)
			// TODO kai send error PG notification
		}
	}()
	log.Printf("[INFO] refreshConnections building connectionUpdates")

	// determine any necessary connection updates
	state.buildConnectionUpdates(ctx)
	defer state.logRefreshConnectionResults()
	// were we successful
	if state.res.Error != nil {
		return
	}

	log.Printf("[INFO] refreshConnections: created connection updates")

	// delete the connection state file - it will be rewritten when we are complete
	log.Printf("[INFO] refreshConnections deleting connections state file")
	steampipeconfig.DeleteConnectionStateFile()
	defer func() {
		if state.res.Error == nil {
			log.Printf("[INFO] refreshConnections saving connections state file")
			steampipeconfig.SaveConnectionStateFile(state.res, state.connectionUpdates)
		}
	}()

	// warn about missing plugins
	state.addMissingPluginWarnings()

	// create object to update the connection state table and notify of state changes
	state.tableUpdater = newConnectionStateTableUpdater(state.connectionUpdates, state.pool)

	// update connectionState table to reflect the updates (i.e. set connections to updating/deleting/ready as appropriate)
	// also this will update the schema hashes of plugins
	if err := state.tableUpdater.start(ctx); err != nil {
		state.res.Error = err
		return
	}

	// if there are no updates, just return
	if !state.connectionUpdates.HasUpdates() {
		log.Println("[INFO] refreshConnections: no updates required")
		return
	}

	log.Printf("[INFO] refreshConnections execute connection queries")

	// execute any necessary queries
	state.executeConnectionQueries(ctx)
	if state.res.Error != nil {
		log.Printf("[INFO] refreshConnections failed with err %s", state.res.Error.Error())
		return
	}

	log.Printf("[INFO] refreshConnections complete")

	state.res.UpdatedConnections = true
}

func (state *refreshConnectionState) buildConnectionUpdates(ctx context.Context) {
	state.connectionUpdates, state.res = steampipeconfig.NewConnectionUpdates(ctx, state.pool, state.forceUpdateConnectionNames...)
}

func (state *refreshConnectionState) addMissingPluginWarnings() {
	log.Printf("[INFO] refreshConnections: identify missing plugins")

	var connectionNames, pluginNames []string
	// add warning if there are connections left over, from missing plugins
	if len(state.connectionUpdates.MissingPlugins) > 0 {
		// warning
		for a, conns := range state.connectionUpdates.MissingPlugins {
			for _, con := range conns {
				connectionNames = append(connectionNames, con.Name)
			}
			pluginNames = append(pluginNames, utils.GetPluginName(a))
		}
		state.res.AddWarning(fmt.Sprintf("%d %s required by %s %s missing. To install, please run %s",
			len(pluginNames),
			utils.Pluralize("plugin", len(pluginNames)),
			utils.Pluralize("connection", len(connectionNames)),
			utils.Pluralize("is", len(pluginNames)),
			constants.Bold(fmt.Sprintf("steampipe plugin install %s", strings.Join(pluginNames, " ")))))
	}
}

func (state *refreshConnectionState) logRefreshConnectionResults() {
	var cmdName = viper.Get(constants.ConfigKeyActiveCommand).(*cobra.Command).Name()
	if cmdName != "plugin-manager" {
		return
	}

	var op strings.Builder
	if state.connectionUpdates != nil {
		op.WriteString(fmt.Sprintf("%s", state.connectionUpdates.String()))
	}
	if state.res != nil {
		op.WriteString(fmt.Sprintf("%s\n", state.res.String()))
	}

	log.Printf("[INFO] refresh connections: \n%s\n", helpers.Tabify(op.String(), "    "))
}

func (state *refreshConnectionState) executeConnectionQueries(ctx context.Context) {
	// retrieve updates from the table updater
	connectionUpdates := state.tableUpdater.updates

	utils.LogTime("db.executeConnectionQueries start")
	defer utils.LogTime("db.executeConnectionQueries start")

	// execute deletions
	state.executeDeleteQueries(ctx)

	// execute updates
	numUpdates := len(connectionUpdates.Update)
	log.Printf("[INFO] executeConnectionQueries: num updates: %d", numUpdates)

	if numUpdates > 0 {
		// get schema queries - this updates schemas for validated plugins and drops schemas for unvalidated plugins
		state.executeUpdateQueries(ctx)
	}

	return
}

// execute all update queries
// NOTE: this only sets res.Error if there is a failure to set update the connection state table
// - all other connection based failures are recorded in the connection state table
func (state *refreshConnectionState) executeUpdateQueries(ctx context.Context) {
	utils.LogTime("db.executeUpdateQueries start")
	defer utils.LogTime("db.executeUpdateQueries end")

	defer func() {
		if state.res.Error != nil {
			log.Printf("[INFO] executeUpdateQueries returned error: %v", state.res.Error)
		}
	}()

	// retrieve updates from the table updater
	connectionUpdates := state.tableUpdater.updates

	// find any plugins which use a newer sdk version than steampipe.
	validationFailures, validatedUpdates, validatedPlugins := steampipeconfig.ValidatePlugins(connectionUpdates.Update, connectionUpdates.ConnectionPlugins)
	if len(validationFailures) > 0 {
		state.res.Warnings = append(state.res.Warnings, steampipeconfig.BuildValidationWarningString(validationFailures))
	}
	numUpdates := len(validatedUpdates)

	// we need to execute the updates in search path order
	// i.e. we first need to update the first search path connection for each plugin (this can be done in parallel)
	// then we can update the remaining connections in parallel
	initialUpdates, remainingUpdates := state.populateInitialAndRemainingUpdates(validatedUpdates)

	exemplarSchemaMap := make(map[string]string)
	log.Printf("[TRACE] executing %d update %s", numUpdates, utils.Pluralize("query", numUpdates))

	// execute initial updates
	// TODO kai parallelizing
	var errors []error
	for connectionName, connectionData := range initialUpdates {

		remoteSchema := utils.PluginFQNToSchemaName(connectionData.Plugin)
		// if this schema is static, add to the exemplar map
		connectionData.CanCloneSchema()
		{
			exemplarSchemaMap[connectionData.Plugin] = connectionName
		}

		// execute update query, and update the connection state table, in a transaction
		sql := db_common.GetUpdateConnectionQuery(connectionName, remoteSchema)

		// the only error this will return is the failure to update the state table
		// - all other errors are written to the state table
		if err := state.executeUpdateQuery(ctx, sql, connectionName); err != nil {
			errors = append(errors, err)
		}
	}

	// if any of the initial schemas failed, do not proceed - these schemas are required to ensure we correctly
	// resolve unqualified queries/tables
	if len(errors) > 0 {
		state.res.Error = error_helpers.CombineErrors(errors...)
		// TODO KAI SEND ERROR NOTIFICATION
		return
	}

	// now that we have updated all exemplar schemars, send postgres notification
	// this gives any attached interactive clients a chance to update their inspect data and autocomplete
	if err := state.sendPostgreSchemaNotification(ctx, state.connectionUpdates.Delete, initialUpdates); err != nil {
		// just log
		log.Printf("[WARN] failed to send schem update Postgres notification: %s", err.Error())
	}

	// now execute remaining
	// TODO KAI wrap this in parallel function which either clones or not
	for connectionName, connectionData := range remainingUpdates {
		remoteSchema := utils.PluginFQNToSchemaName(connectionData.Plugin)
		// execute update query, and update the connection state table, in a transaction
		sql := db_common.GetUpdateConnectionQuery(connectionName, remoteSchema)

		// the only error this will return is the failure to update the state table
		// - all other errors are written to the state table
		if err := state.executeUpdateQuery(ctx, sql, connectionName); err != nil {
			errors = append(errors, err)
		}
	}
	if len(errors) > 0 {
		state.res.Error = error_helpers.CombineErrors(errors...)
	}
	//	statushooks.SetStatus(ctx, fmt.Sprintf("Cloning %d %s", len(cloneableConnections), utils.Pluralize("connection", len(cloneableConnections))))
	//	if err := cloneConnectionSchemas(ctx, state.pool, exemplarSchemaMap, cloneableConnections, idx, numUpdates, state.tableUpdater); err != nil {
	//		res.Error = err
	//		return state.res
	//	}
	//}

	log.Printf("[TRACE] all update queries executed")

	for _, failure := range validationFailures {
		log.Printf("[TRACE] remove schema for connection failing validation connection %s, plugin Name %s\n ", failure.ConnectionName, failure.Plugin)
		if failure.ShouldDropIfExists {
			_, err := state.pool.Exec(ctx, db_common.GetDeleteConnectionQuery(failure.ConnectionName))
			if err != nil {
				// NOTE: do not return an error if we fail to remove an invalid connection - just log it
				log.Printf("[WARN] failed to delete invalid connection '%s' (%s) : %s", failure.ConnectionName, failure.Message, err.Error())
			}
		}
	}

	if viper.GetBool(constants.ArgSchemaComments) {
		state.writeComments(ctx, validatedPlugins)
	}

	log.Printf("[TRACE] executeUpdateQueries complete")
	return
}

func (state *refreshConnectionState) populateInitialAndRemainingUpdates(validatedUpdates steampipeconfig.ConnectionStateMap) (initialUpdates, remainingUpdates steampipeconfig.ConnectionStateMap) {
	searchPathConnections := state.connectionUpdates.FinalConnectionState.GetFirstSearchPathConnectionForPlugins(state.searchPath)
	initialUpdates = make(steampipeconfig.ConnectionStateMap)
	remainingUpdates = make(steampipeconfig.ConnectionStateMap)

	// convert this into a lookup of initial updates to execute
	for _, connectionName := range searchPathConnections {
		if connectionState, updateRequired := validatedUpdates[connectionName]; updateRequired {
			initialUpdates[connectionName] = connectionState
		}
	}
	// now add remaining updates to remainingUpdates
	for connectionName, connectionState := range validatedUpdates {
		if _, isInitialUpdate := initialUpdates[connectionName]; !isInitialUpdate {
			remainingUpdates[connectionName] = connectionState
		}

	}
	return initialUpdates, remainingUpdates
}

func (state *refreshConnectionState) writeComments(ctx context.Context, validatedPlugins map[string]*steampipeconfig.ConnectionPlugin) {
	log.Printf("[WARN] start comments")

	conn, err := state.pool.Acquire(ctx)
	if err != nil {
		// NOTE: do not return an error if we fail to write comments
		log.Printf("[WARN] failed to write comments: could not acquire connection: %s", err.Error())
		return
	}
	defer conn.Release()

	numCommentsUpdates := len(validatedPlugins)
	log.Printf("[TRACE] executing %d comment %s", numCommentsUpdates, utils.Pluralize("query", numCommentsUpdates))

	for connectionName, connectionPlugin := range validatedPlugins {
		// check this connection has not failed
		if _, connectionFailed := state.res.FailedConnections[connectionName]; connectionFailed {
			continue
		}
		_, err = db_local.ExecuteSqlInTransaction(ctx, conn.Conn(), "lock table pg_namespace;", db_common.GetCommentsQueryForPlugin(connectionName, connectionPlugin.ConnectionMap[connectionName].Schema.Schema))
		if err != nil {
			// NOTE: do not return an error if we fail to write comments
			log.Printf("[WARN] failed to write comments for connection '%s': %s", connectionName, err.Error())
		}
		// TODO KAI update connection state
	}
}

func (state *refreshConnectionState) executeUpdateQuery(ctx context.Context, sql, connectionName string) error {
	// create a transaction
	tx, err := state.pool.Begin(ctx)
	if err != nil {
		return sperr.WrapWithMessage(err, "failed to create transaction to perform update query")
	}
	defer func() {
		if err != nil {
			tx.Rollback(ctx)
		} else {
			tx.Commit(ctx)
		}
	}()

	// TODO KAI HACK
	//if connectionName == "aws_015" {
	//	statusErr := state.tableUpdater.onConnectionError(ctx, tx, connectionName, fmt.Errorf("HACKETY"))
	//	// update failed connections in result
	//	state.res.AddFailedConnection(connectionName, err.Error())
	//
	//	// NOTE: do not return the error - unless we failed to update the connection state table
	//	if statusErr != nil {
	//		return error_helpers.CombineErrorsWithPrefix(fmt.Sprintf("failed to update connectionm %s and failed to update connection_state table", connectionName), err, statusErr)
	//	}
	//	return nil
	//}

	// execute update sql
	_, err = tx.Exec(ctx, sql)
	if err != nil {
		statusErr := state.tableUpdater.onConnectionError(ctx, tx, connectionName, err)
		// update failed connections in result
		state.res.AddFailedConnection(connectionName, err.Error())

		// NOTE: do not return the error - unless we failed to update the connection state table
		if statusErr != nil {
			return error_helpers.CombineErrorsWithPrefix(fmt.Sprintf("failed to update connection %s and failed to update connection_state table", connectionName), err, statusErr)
		}
		return nil
	}

	// update state table (inside transaction)
	err = state.tableUpdater.onConnectionReady(ctx, tx, connectionName)
	if err != nil {
		return sperr.WrapWithMessage(err, "failed to update connection state table")
	}
	return nil
}

func (state *refreshConnectionState) executeDeleteQueries(ctx context.Context) error {
	deletions := maps.Keys(state.connectionUpdates.Delete)
	statushooks.SetStatus(ctx, fmt.Sprintf("Deleting %d %s", len(deletions), utils.Pluralize("connection", len(deletions))))
	var errors []error
	for _, c := range deletions {
		utils.LogTime("delete connection start")
		log.Printf("[TRACE] delete connection %s\n ", c)

		err := state.executeDeleteQuery(ctx, c)
		if err != nil {
			errors = append(errors, err)
		}
		utils.LogTime("delete connection end")
	}

	return error_helpers.CombineErrors(errors...)
}

// delete the schema and update remove the connection from the state table
// NOTE: this only returns an error if we fail to update the state table
func (state *refreshConnectionState) executeDeleteQuery(ctx context.Context, connectionName string) error {
	sql := db_common.GetDeleteConnectionQuery(connectionName)
	// create a transaction
	tx, err := state.pool.Begin(ctx)
	if err != nil {
		return sperr.WrapWithMessage(err, "failed to create transaction to perform delete query")
	}
	defer func() {
		if err != nil {
			tx.Rollback(ctx)
		} else {
			tx.Commit(ctx)
		}
	}()

	// execute delete sql
	_, err = tx.Exec(ctx, sql)
	if err != nil {
		statusErr := state.tableUpdater.onConnectionError(ctx, tx, connectionName, err)
		// NOTE: do not return the error - unless we failed to update the connection state table
		if statusErr != nil {
			return error_helpers.CombineErrorsWithPrefix(fmt.Sprintf("failed to update connectionm %s and failed to update connection_state table", connectionName), err, statusErr)
		}
		return nil
	}

	// delete state table entry (inside transaction)
	err = state.tableUpdater.onConnectionDeleted(ctx, tx, connectionName)
	if err != nil {
		return sperr.WrapWithMessage(err, "failed to delete connection state table entry for '%s'", connectionName)
	}
	return nil
}

// sett the state of all connections to error
func (state *refreshConnectionState) setAllConnectionStateToError(ctx context.Context, err error) {
	// create wrapped error
	connectionStateError := sperr.WrapWithMessage(err, "failed to update Steampipe connections")
	// load connection state
	conn, err := state.pool.Acquire(ctx)
	if err != nil {
		log.Printf("[WARN] setAllConnectionStateToError failed to acquire connection from pool: %s", err.Error())
		return
	}
	defer conn.Release()

	// load the connection state file and filter out any connections which are not in the list of schemas
	// this allows for the database being rebuilt,modified externally
	currentConnectionState, err := steampipeconfig.LoadConnectionState(ctx, conn.Conn())
	if err != nil {
		log.Printf("[WARN] setAllConnectionStateToError failed to load connection state: %s", err.Error())
		return
	}
	var queries []db_common.QueryWithArgs
	for name := range currentConnectionState {
		queries = append(queries, getConnectionStateErrorSql(name, connectionStateError))
	}

	if _, err = db_local.ExecuteSqlWithArgsInTransaction(ctx, conn.Conn(), queries...); err != nil {
		log.Printf("[WARN] setAllConnectionStateToError failed to set connection state to error: %s", err.Error())
		return
	}
}

func (state *refreshConnectionState) cloneConnectionSchemas(ctx context.Context, pluginMap map[string]string, cloneableConnections steampipeconfig.ConnectionStateMap, idx int, numUpdates int) error {
	var wg sync.WaitGroup
	var progressChan = make(chan string)
	type connectionError struct {
		name string
		err  error
	}
	var errChan = make(chan connectionError)

	var pluginMapMut sync.Mutex

	sem := semaphore.NewWeighted(int64(state.pool.Config().MaxConns))
	var errors []error

	go func() {
		for {
			select {
			case connectionError := <-errChan:
				errors = append(errors, connectionError.err)
				state.tableUpdater.onConnectionError(ctx, nil, connectionError.name, connectionError.err)
			case connectionName := <-progressChan:
				if connectionName == "" {
					return
				}
				idx++

			}
		}
	}()
	for n, d := range cloneableConnections {
		wg.Add(1)
		if err := sem.Acquire(ctx, 1); err != nil {
			return err
		}
		// use semaphore to limit goroutines
		go func(connectionName string, connectionData *steampipeconfig.ConnectionState) {
			//log.Printf("[WARN] start clone connection %s", connectionName)
			defer func() {
				wg.Done()
				sem.Release(1)
			}()

			// this schema is already in the plugin map, clone from it
			exemplarSchemaName := pluginMap[connectionData.Plugin]

			// Clone the foreign schema into this connection.
			sql := fmt.Sprintf("select clone_foreign_schema('%s', '%s', '%s');", exemplarSchemaName, connectionName, connectionData.Plugin)
			// execute clone query, and update the connection state table, in a transaction
			if err := state.executeUpdateQuery(ctx, sql, connectionName); err != nil {
				errChan <- connectionError{connectionName, err}
				return
			}

			pluginMapMut.Lock()
			pluginMap[connectionData.Plugin] = connectionName
			pluginMapMut.Unlock()

			progressChan <- connectionName
		}(n, d)

	}

	wg.Wait()
	close(progressChan)

	return error_helpers.CombineErrors(errors...)
}

// OnConnectionsChanged is the callback function invoked by the connection watcher when connections are added or removed
func (state *refreshConnectionState) sendPostgreSchemaNotification(ctx context.Context, deletions map[string]struct{}, updates steampipeconfig.ConnectionStateMap) error {
	conn, err := db_local.CreateLocalDbConnection(ctx, &db_local.CreateDbOptions{Username: constants.DatabaseSuperUser})
	if err != nil {
		log.Printf("[WARN] failed to send schema update notification: %s", err)
	}

	notification := steampipeconfig.NewSchemaUpdateNotification(
		maps.Keys(updates),
		maps.Keys(deletions))

	return db_local.SendPostgresNotification(ctx, conn, notification)
}