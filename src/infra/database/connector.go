package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// openPool opens a database/sql pool over pgx whose argument-free queries run
// on the simple protocol.
//
// bun renders argument values into the SQL text and sends no bind args, so
// almost every query text it produces is unique. Under pgx's default
// QueryExecModeCacheStatement each new text costs an extra Parse/Describe
// round trip and leaves a server-side prepared statement behind on the
// connection. The simple protocol runs such a query in one round trip and
// keeps nothing on the server. Parameterised queries (River's) keep the
// default extended-protocol mode, so their argument encoding is unchanged.
func openPool(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	return sql.OpenDB(simpleQueryConnector{stdlib.GetConnector(*cfg)}), nil
}

type simpleQueryConnector struct {
	driver.Connector
}

func (c simpleQueryConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	pc, ok := conn.(*stdlib.Conn)
	if !ok {
		return conn, nil
	}
	return &simpleQueryConn{Conn: pc}, nil
}

// simpleQueryConn embeds *stdlib.Conn so database/sql still sees every
// optional driver interface it implements (transactions, session reset,
// named-value checks), and only overrides QueryContext.
type simpleQueryConn struct {
	*stdlib.Conn
}

// simpleProtocolArg asks pgx to run the query on the simple protocol; pgx
// consumes it as a query option, not as a bind parameter.
var simpleProtocolArg = []driver.NamedValue{{Ordinal: 1, Value: pgx.QueryExecModeSimpleProtocol}}

func (c *simpleQueryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if len(args) == 0 {
		args = simpleProtocolArg
	}
	return c.Conn.QueryContext(ctx, query, args)
}
