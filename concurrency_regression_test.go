package figo_test

import (
	. "github.com/bi0dread/figo/v4"
	. "github.com/bi0dread/figo/v4/adapters"

	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Run with -race: SetPluginManager concurrent with AddFiltersFromString.
func TestPluginManagerRace(t *testing.T) {
	f := New()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = f.AddFiltersFromString(`a=1`)
		}()
		go func() {
			defer wg.Done()
			f.SetPluginManager(NewPluginManager())
		}()
	}
	wg.Wait()
}

// Run with -race: SetRegexSQLOperator concurrent with rendering.
func TestRegexOperatorRace(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`name=~"^a"`))
	f.Build(RawAdapter{})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			SetRegexSQLOperator("REGEXP")
		}()
		go func() {
			defer wg.Done()
			_ = f.GetSqlString(RawContext{Table: "t"})
		}()
	}
	wg.Wait()
	SetRegexSQLOperator("REGEXP") // restore default for other tests
}

// Run with -race: Walk (even a no-op visitor) concurrent with rendering must
// not write into operand arrays shared with reader snapshots.
func TestWalkDoesNotRaceWithReaders(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`id=1 and (age>20 or active=true)`))
	f.Build(RawAdapter{})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			f.Walk(func(Expr) {})
		}()
		go func() {
			defer wg.Done()
			_ = f.GetSqlString(RawContext{Table: "t"})
		}()
		go func() {
			defer wg.Done()
			_ = f.Explain()
		}()
	}
	wg.Wait()
}

// A Walk visitor may call figo methods without deadlocking.
func TestWalkVisitorMayCallFigoMethods(t *testing.T) {
	f := New()
	require.NoError(t, f.AddFiltersFromString(`id=1`))
	f.Build(RawAdapter{})

	done := make(chan struct{})
	go func() {
		f.Walk(func(Expr) {
			_ = f.GetPage()
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Walk visitor calling f.GetPage() deadlocked")
	}
}

type reentrantPlugin struct {
	pm *PluginManager
}

func (p *reentrantPlugin) Name() string                { return "reentrant" }
func (p *reentrantPlugin) Version() string             { return "1" }
func (p *reentrantPlugin) Initialize(Figo) error       { return nil }
func (p *reentrantPlugin) BeforeQuery(Figo, any) error { return nil }
func (p *reentrantPlugin) AfterQuery(Figo, any, interface{}) error {
	return nil
}
func (p *reentrantPlugin) BeforeParse(f Figo, dsl string) (string, error) {
	// Calling back into the manager from a hook must not deadlock.
	_ = p.pm.ListPlugins()
	return dsl, nil
}
func (p *reentrantPlugin) AfterParse(Figo, string) error { return nil }

func TestPluginHookMayCallManager(t *testing.T) {
	f := New()
	pm := NewPluginManager()
	plugin := &reentrantPlugin{pm: pm}
	require.NoError(t, pm.RegisterPlugin(plugin))
	f.SetPluginManager(pm)

	done := make(chan struct{})
	go func() {
		_ = f.AddFiltersFromString(`a=1`)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("plugin hook calling back into manager deadlocked")
	}
}

type failingInitPlugin struct{}

func (failingInitPlugin) Name() string                            { return "failing" }
func (failingInitPlugin) Version() string                         { return "1" }
func (failingInitPlugin) Initialize(Figo) error                   { return assert.AnError }
func (failingInitPlugin) BeforeQuery(Figo, any) error             { return nil }
func (failingInitPlugin) AfterQuery(Figo, any, interface{}) error { return nil }
func (failingInitPlugin) BeforeParse(_ Figo, dsl string) (string, error) {
	return dsl, nil
}
func (failingInitPlugin) AfterParse(Figo, string) error { return nil }

// A plugin whose Initialize fails must not stay registered.
func TestRegisterPluginRollsBackOnInitError(t *testing.T) {
	f := New()
	require.Error(t, f.RegisterPlugin(failingInitPlugin{}))

	pm := f.GetPluginManager()
	require.NotNil(t, pm)
	_, found := pm.GetPlugin("failing")
	assert.False(t, found, "failed plugin must be rolled back")

	// And re-registering (e.g. after fixing the failure cause) must work.
	assert.Error(t, f.RegisterPlugin(failingInitPlugin{}), "expect same init error, not 'already registered'")
}

// reentrantFilter calls back into the Figo instance from FilterExpr — legal
// because Build/AddFilter run expression filters outside the instance lock.
type reentrantFilter struct{ failingInitPlugin }

func (reentrantFilter) Name() string          { return "reentrant-filter" }
func (reentrantFilter) Initialize(Figo) error { return nil }
func (reentrantFilter) FilterExpr(f Figo, e Expr) Expr {
	_ = f.GetNamingFunc() // read call-back must not deadlock
	_ = f.GetSelectFields()
	return e
}

// A FilterExpr callback may call back into the instance without deadlocking.
func TestExprFilterMayCallBackIntoFigo(t *testing.T) {
	f := New()
	require.NoError(t, f.RegisterPlugin(reentrantFilter{}))

	done := make(chan struct{})
	go func() {
		require.NoError(t, f.AddFiltersFromString(`a=1 and b=2`))
		f.Build(RawAdapter{})
		f.AddFilter(EqExpr{Field: "c", Value: int64(3)})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("FilterExpr calling back into Figo deadlocked")
	}
	assert.Len(t, f.GetClauses(), 2)
}
