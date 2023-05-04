package db_client

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sethvargo/go-retry"
	typehelpers "github.com/turbot/go-kit/types"
	"github.com/turbot/steampipe/pkg/constants"
	"github.com/turbot/steampipe/pkg/statushooks"
	"github.com/turbot/steampipe/pkg/steampipeconfig"
	"regexp"
	"time"
)

// execute query - if it fails with a "relation not found" error, determine whether this is because the required schema
// has not yet loaded and if so, wait for it to load and retry
func (c *DbClient) startQueryWithRetries(ctx context.Context, conn *pgx.Conn, query string, args ...any) (pgx.Rows, error) {
	// long timeout to give refresh connections a chance to finish
	maxDuration := 10 * time.Minute
	backoffInterval := 250 * time.Millisecond
	backoff := retry.NewConstant(backoffInterval)

	var res pgx.Rows
	err := retry.Do(ctx, retry.WithMaxDuration(maxDuration, backoff), func(ctx context.Context) error {
		rows, queryError := c.startQuery(ctx, conn, query, args...)
		// if there is no error, just return
		if queryError == nil {
			statushooks.SetStatus(ctx, "Loading results...")
			res = rows
			return nil
		}

		// so there is an error - is it "relation not found"?
		missingSchema, _, relationNotFound := isRelationNotFoundError(queryError)
		if !relationNotFound {
			// just return it
			return queryError
		}
		// so this _was_ a "relation not found" error
		// load the connection state and connection config to see if the missing schema is in there at all
		// if there was a schema not found with an unqualified query, we keep trying until
		// the first search path schema for each plugin has loaded

		connectionStateMap, stateErr := steampipeconfig.LoadConnectionState(ctx, conn, steampipeconfig.WithWaitUntilLoading())
		if stateErr != nil {
			// just return the query error
			return queryError
		}
		// if there are no connections, just return the error
		if len(connectionStateMap) == 0 {
			return queryError
		}

		// is this an unqualified query...
		if missingSchema == "" {
			// if all connections are ready (and have been for more than the backoff interval) , just return the relation not found error
			if connectionStateMap.Loaded() && time.Since(connectionStateMap.ConnectionModTime()) > backoffInterval {
				return queryError
			}

			// tell our client to reload the search path, as now the connection state is in loading state,
			// search paths may have been updated
			if err := c.loadUserSearchPath(ctx, conn); err != nil {
				return queryError
			}
			c.SetRequiredSessionSearchPath(ctx)

			// TODO KAI test this
			// otherwise we need to wait for the first schema of everything plugin to load
			if _, err := steampipeconfig.LoadConnectionState(ctx, conn, steampipeconfig.WithWaitForSearchPath(c.GetRequiredSessionSearchPath())); err != nil {
				return err
			}
			return retry.RetryableError(queryError)
		}

		// so a schema was specified
		// verify it exists in the connection state
		connectionState, missingSchemaExistsInStateMap := connectionStateMap[missingSchema]
		if !missingSchemaExistsInStateMap {
			//, missing schema is not in connection state map - just return the error
			return queryError
		}

		// so schema _is_ in the state map

		// if the connection is ready (and has been for more than the backoff interval) , just return the relation not found error
		if connectionState.State == constants.ConnectionStateReady && time.Since(connectionState.ConnectionModTime) > backoffInterval {
			return queryError
		}

		// if connection is in error,return the connection error
		if connectionState.State == constants.ConnectionStateError {
			return fmt.Errorf("connection %s failed to load: %s", missingSchema, typehelpers.SafeString(connectionState.ConnectionError))
		}

		// ok so we will retry
		// build the status message to display with a spinner, if needed
		statusMessage := steampipeconfig.GetLoadingConnectionStatusMessage(connectionStateMap, missingSchema)
		statushooks.SetStatus(ctx, statusMessage)
		return retry.RetryableError(queryError)
	})

	return res, err
}

func isRelationNotFoundError(err error) (string, string, bool) {
	if err == nil {
		return "", "", false
	}
	pgErr, ok := err.(*pgconn.PgError)
	if !ok || pgErr.Code != "42P01" {
		return "", "", false
	}

	r := regexp.MustCompile(`^relation "(.*)\.(.*)" does not exist$`)
	captureGroups := r.FindStringSubmatch(pgErr.Message)
	if len(captureGroups) == 3 {

		return captureGroups[1], captureGroups[2], true
	}

	// maybe there is no schema
	r = regexp.MustCompile(`^relation "(.*)" does not exist$`)
	captureGroups = r.FindStringSubmatch(pgErr.Message)
	if len(captureGroups) == 2 {
		return "", captureGroups[1], true
	}
	return "", "", true
}
