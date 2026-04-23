package internal

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
	"modernc.org/sqlite"
)

var dbDialects sync.Map

func registerDBDialect(db *sql.DB, dialect Dialect) {
	if db == nil {
		return
	}
	dbDialects.Store(db, dialect)
}

func unregisterDBDialect(db *sql.DB) {
	if db == nil {
		return
	}
	dbDialects.Delete(db)
}

func dialectForDB(db *sql.DB) Dialect {
	if db == nil {
		return DialectSQLite
	}
	if dialect, ok := dbDialects.Load(db); ok {
		return dialect.(Dialect)
	}
	return DialectSQLite
}

func rebindSQL(dialect Dialect, query string) string {
	if dialect != DialectPostgres || !strings.Contains(query, "?") {
		return query
	}

	var out strings.Builder
	out.Grow(len(query) + 8)

	argNum := 1
	inSingleQuote := false
	inDoubleQuote := false
	inLineComment := false
	inBlockComment := false

	for i := 0; i < len(query); i++ {
		ch := query[i]

		if inLineComment {
			out.WriteByte(ch)
			if ch == '\n' {
				inLineComment = false
			}
			continue
		}

		if inBlockComment {
			out.WriteByte(ch)
			if ch == '*' && i+1 < len(query) && query[i+1] == '/' {
				out.WriteByte('/')
				i++
				inBlockComment = false
			}
			continue
		}

		if inSingleQuote {
			out.WriteByte(ch)
			if ch == '\'' {
				if i+1 < len(query) && query[i+1] == '\'' {
					out.WriteByte('\'')
					i++
				} else {
					inSingleQuote = false
				}
			}
			continue
		}

		if inDoubleQuote {
			out.WriteByte(ch)
			if ch == '"' {
				if i+1 < len(query) && query[i+1] == '"' {
					out.WriteByte('"')
					i++
				} else {
					inDoubleQuote = false
				}
			}
			continue
		}

		if ch == '-' && i+1 < len(query) && query[i+1] == '-' {
			out.WriteString("--")
			i++
			inLineComment = true
			continue
		}

		if ch == '/' && i+1 < len(query) && query[i+1] == '*' {
			out.WriteString("/*")
			i++
			inBlockComment = true
			continue
		}

		switch ch {
		case '\'':
			inSingleQuote = true
			out.WriteByte(ch)
		case '"':
			inDoubleQuote = true
			out.WriteByte(ch)
		case '?':
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(argNum))
			argNum++
		default:
			out.WriteByte(ch)
		}
	}

	return out.String()
}

func execDB(db *sql.DB, query string, args ...any) (sql.Result, error) {
	return db.Exec(rebindSQL(dialectForDB(db), query), args...)
}

func queryDB(db *sql.DB, query string, args ...any) (*sql.Rows, error) {
	return db.Query(rebindSQL(dialectForDB(db), query), args...)
}

func queryRowDB(db *sql.DB, query string, args ...any) *sql.Row {
	return db.QueryRow(rebindSQL(dialectForDB(db), query), args...)
}

type Tx struct {
	*sql.Tx
	dialect Dialect
}

func beginTx(db *sql.DB) (*Tx, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, dialect: dialectForDB(db)}, nil
}

func (tx *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.Tx.Exec(rebindSQL(tx.dialect, query), args...)
}

func (tx *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.Tx.Query(rebindSQL(tx.dialect, query), args...)
}

func (tx *Tx) QueryRow(query string, args ...any) *sql.Row {
	return tx.Tx.QueryRow(rebindSQL(tx.dialect, query), args...)
}

func (s *Store) Rebind(query string) string {
	if s == nil {
		return query
	}
	return rebindSQL(s.Dialect, query)
}

func (s *Store) Exec(query string, args ...any) (sql.Result, error) {
	return s.DB.Exec(s.Rebind(query), args...)
}

func (s *Store) Query(query string, args ...any) (*sql.Rows, error) {
	return s.DB.Query(s.Rebind(query), args...)
}

func (s *Store) QueryRow(query string, args ...any) *sql.Row {
	return s.DB.QueryRow(s.Rebind(query), args...)
}

func (s *Store) Begin() (*Tx, error) {
	if s == nil {
		return nil, sql.ErrConnDone
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, dialect: s.Dialect}, nil
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return true
	}

	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		lower := strings.ToLower(sqliteErr.Error())
		return strings.Contains(lower, "unique")
	}

	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "unique constraint") ||
		strings.Contains(lower, "duplicate key") ||
		strings.Contains(lower, "unique")
}
