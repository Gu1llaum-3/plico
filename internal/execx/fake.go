package execx

import (
	"context"
	"fmt"
	"sync"
)

// Response is one scripted answer of a FakeRunner.
type Response struct {
	Result Result
	Err    error
}

// FakeRunner is a test double: it records every call and answers either from
// a sequential Script or from a Match predicate.
type FakeRunner struct {
	mu     sync.Mutex
	Calls  []Cmd
	Script []Response
	Match  func(Cmd) (Result, error)
	next   int
}

func (f *FakeRunner) Run(ctx context.Context, c Cmd) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Result{ExitCode: -1}, err
	}
	f.Calls = append(f.Calls, c)
	if f.Match != nil {
		return f.Match(c)
	}
	if f.next < len(f.Script) {
		r := f.Script[f.next]
		f.next++
		return r.Result, r.Err
	}
	return Result{}, fmt.Errorf("execx.FakeRunner: unexpected call %q %v", c.Name, c.Args)
}
