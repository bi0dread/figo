package plugins

import (
	"fmt"
	"strings"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"github.com/bi0dread/figo/v4/adapters"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Whether figo is injectable through sort= or through a field name is not a
// question to answer by reading the renderer. These tests EXECUTE every payload
// against a real database and check the database afterwards.
//
// The reported reason to care: filters=...sort=median_price:desc+or+1=1 was
// called SQL injection. It is not — but the only convincing proof is a table
// that is still there, with the rows it started with, after the payload runs.

type injRow struct {
	ID     uint `gorm:"primarykey"`
	Name   string
	Secret string
	Tenant int
}

func (injRow) TableName() string { return "inj_rows" }

func injDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&injRow{}))
	for i := 1; i <= 3; i++ {
		require.NoError(t, db.Create(&injRow{
			Name:   fmt.Sprintf("row%d", i),
			Secret: "classified",
			Tenant: i,
		}).Error)
	}
	return db
}

// injRun executes dsl and reports what the database actually did.
type injResult struct {
	addErr  error  // what AddFiltersFromString said
	sql     string // the statement GORM built
	execErr error  // what the driver said
	rows    int    // rows returned
	total   int64  // rows STILL IN THE TABLE afterwards
	dropped bool   // did the table survive at all
}

func injRun(t *testing.T, dsl string, guard bool) injResult {
	t.Helper()
	db := injDB(t)

	f := figo.New()
	f.SetNamingFunc(figo.NoChangeNaming)
	if guard {
		require.NoError(t, f.RegisterPlugin(NewInjectionGuardPlugin()))
	}

	res := injResult{}
	res.addErr = f.AddFiltersFromString(dsl)
	f.Build(adapters.GormAdapter{}) // a caller that IGNORES addErr

	var got []injRow
	tx := adapters.ApplyGorm(f, db.Session(&gorm.Session{DryRun: true}).Model(&injRow{}))
	if stmt := tx.Find(&got).Statement; stmt != nil {
		res.sql = stmt.SQL.String()
	}

	// Now for real.
	err := adapters.ApplyGorm(f, db.Model(&injRow{})).Find(&got).Error
	res.execErr = err
	res.rows = len(got)

	// The evidence that matters: is the table still there, with its rows?
	if e := db.Model(&injRow{}).Count(&res.total).Error; e != nil {
		res.dropped = true
	}
	return res
}

// ---------------------------------------------------------------------------
// sort=
// ---------------------------------------------------------------------------

// Every payload here goes into the sort= directive, which is where the report
// put its own. None of them injects: figo renders a sort key as ONE quoted
// identifier, so the payload becomes a column name that does not exist, and the
// engine says so.
func TestSortDirectiveIsNotInjectable(t *testing.T) {
	payloads := []string{
		`sort=median_price:desc or 1=1`,          // the reported one
		`sort=name:desc; DROP TABLE inj_rows--`,  // statement terminator
		`sort=name:desc--`,                       // comment out the rest
		`sort=name:desc/*x*/`,                    // block comment
		"sort=name`:desc",                        // backtick breakout
		`sort=name":desc`,                        // double-quote breakout
		`sort=name':desc`,                        // single-quote breakout
		`sort=(SELECT secret FROM inj_rows):asc`, // subquery
		`sort=name:desc,(select 1):asc`,          // subquery as 2nd key
		`sort=name) OR (1=1:desc`,                // paren breakout
		`sort=name:desc UNION SELECT secret FROM inj_rows`,
		`sort=1:desc`, // the bare literal
		`sort=name:desc; UPDATE inj_rows SET secret='pwned'--`,
	}

	for _, dsl := range payloads {
		t.Run(dsl, func(t *testing.T) {
			bare := injRun(t, dsl, false)

			// The table is untouched no matter what, WITHOUT the plugin.
			assert.False(t, bare.dropped, "the table was dropped")
			assert.EqualValues(t, 3, bare.total, "rows were added or deleted")

			// No payload ever became a second statement or a subquery: either
			// the parser refused the segment (no ORDER BY at all) or the whole
			// thing is one backtick-quoted identifier.
			if strings.Contains(bare.sql, "ORDER BY") {
				order := bare.sql[strings.Index(bare.sql, "ORDER BY"):]
				assert.NotContains(t, strings.ToUpper(order), "SELECT",
					"a subquery reached ORDER BY: %s", bare.sql)
				assert.NotContains(t, order, ";", "a second statement reached ORDER BY: %s", bare.sql)
				assert.NotContains(t, order, "--", "a comment reached ORDER BY: %s", bare.sql)
			}

			// Secrets are still secret.
			var pwned int64
			require.NoError(t, injDB(t).Model(&injRow{}).Where("secret = ?", "pwned").Count(&pwned).Error)
			assert.EqualValues(t, 0, pwned)

			// With the guard, the request is refused outright and the query
			// returns nothing rather than a driver error quoting the statement.
			g := injRun(t, dsl, true)
			assert.Error(t, g.addErr, "the guard accepted %q", dsl)
			assert.Equal(t, 0, g.rows)
			assert.False(t, g.dropped)
			assert.EqualValues(t, 3, g.total)
		})
	}
}

// ---------------------------------------------------------------------------
// field names
// ---------------------------------------------------------------------------

// The same payloads in the FIELD-NAME position, which is where the reported
// `1=1` actually landed.
func TestFieldNameIsNotInjectable(t *testing.T) {
	payloads := []string{
		`1=1`,                          // the reported shape
		`name' OR '1'='1=x`,            // classic quote breakout
		"name`=1",                      // backtick breakout
		`name;DROP TABLE inj_rows--=1`, // terminator + comment
		`name--=1`,                     // comment
		`name/*x*/=1`,                  // block comment
		`name) OR (1=1`,                // paren breakout
		`secret=x' UNION SELECT secret FROM inj_rows--`,
		`tenant=1 OR 1=1`, // the textbook payload, spaced
	}

	for _, dsl := range payloads {
		t.Run(dsl, func(t *testing.T) {
			bare := injRun(t, dsl, false)

			assert.False(t, bare.dropped, "the table was dropped")
			assert.EqualValues(t, 3, bare.total)

			// Nothing became a second statement, a comment, or a union. The
			// WHERE clause is identifiers and bound placeholders only.
			if i := strings.Index(bare.sql, "WHERE"); i >= 0 {
				where := bare.sql[i:]
				assert.NotContains(t, strings.ToUpper(where), "UNION", bare.sql)
				assert.NotContains(t, strings.ToUpper(where), "DROP", bare.sql)
				assert.NotContains(t, where, ";", bare.sql)
			}

			// And it never returned MORE rows than an honest filter would.
			assert.LessOrEqual(t, bare.rows, 3)

			g := injRun(t, dsl, true)
			assert.Error(t, g.addErr, "the guard accepted %q", dsl)
			assert.Equal(t, 0, g.rows, "the refused query returned rows")
			assert.NoError(t, g.execErr, "the refused query still reached the engine as an error")
			assert.EqualValues(t, 3, g.total)
		})
	}
}

// ---------------------------------------------------------------------------
// values
// ---------------------------------------------------------------------------

// The value position is where injection would normally live, and it is bound.
// These must all run CLEANLY and match nothing — a payload as a value is just
// a string to compare against.
func TestValuesAreBoundAndNeverInjected(t *testing.T) {
	payloads := []string{
		`name="' OR '1'='1"`,
		`name="'; DROP TABLE inj_rows--"`,
		`name="1=1"`,
		`name="x' UNION SELECT secret FROM inj_rows--"`,
		"name=\"`inj_rows`\"",
		`name="%' OR name LIKE '%"`,
	}

	for _, dsl := range payloads {
		t.Run(dsl, func(t *testing.T) {
			res := injRun(t, dsl, false)

			assert.NoError(t, res.addErr)
			assert.NoError(t, res.execErr, "a bound value should never fail to execute")
			assert.Equal(t, 0, res.rows, "a payload value matched a row")
			assert.False(t, res.dropped)
			assert.EqualValues(t, 3, res.total)
			assert.Contains(t, res.sql, "?", "the value was not bound: %s", res.sql)

			// The guard must NOT refuse these: it screens field names, never
			// values. A search box containing a quote is not an attack.
			g := injRun(t, dsl, true)
			assert.NoError(t, g.addErr, "the guard refused a value: %q", dsl)
			assert.Equal(t, res.sql, g.sql, "the guard changed a legitimate query")
		})
	}
}

// ---------------------------------------------------------------------------
// what the report actually hit
// ---------------------------------------------------------------------------

// The reported request, end to end, on a real database — showing precisely what
// went wrong and what the guard changes. Not an injection: a driver error whose
// text quotes the whole statement.
func TestReportedRequestEndToEnd(t *testing.T) {
	const reported = `page=skip:0,take:50 and sort=median_price:desc or 1=1`

	bare := injRun(t, reported, false)
	require.NoError(t, bare.addErr, "figo used to report nothing at all")
	assert.Contains(t, bare.sql, "`1`", "the invented column is a quoted identifier, not a literal")
	require.Error(t, bare.execErr, "the engine has to refuse a column that does not exist")
	assert.Contains(t, bare.execErr.Error(), "no such column",
		"this is the error whose text leaked the statement")
	assert.Equal(t, 0, bare.rows)
	assert.EqualValues(t, 3, bare.total, "nothing was executed")

	guarded := injRun(t, reported, true)
	require.Error(t, guarded.addErr, "the caller finally has something to reject on")
	assert.Contains(t, guarded.addErr.Error(), `filter field "1"`)
	assert.NoError(t, guarded.execErr, "no driver error means nothing to leak")
	assert.Equal(t, 0, guarded.rows, "and the refusal returns nothing, not everything")
	assert.Contains(t, guarded.sql, "1=0")
	assert.EqualValues(t, 3, guarded.total)
}
