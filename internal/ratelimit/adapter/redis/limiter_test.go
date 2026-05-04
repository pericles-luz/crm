package redis_test

import (
	"context"
	"errors"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/ratelimit"
	redisadapter "github.com/pericles-luz/crm/internal/ratelimit/adapter/redis"
)

// fakeRediser implements redisadapter.Rediser. Only the EvalSha + Eval
// methods are exercised by redis_rate's Script.Run, so the others return
// error Cmd values to surface unexpected calls during tests.
type fakeRediser struct {
	evalShaResult *goredis.Cmd
	evalResult    *goredis.Cmd
	delResult     *goredis.IntCmd
}

func (f *fakeRediser) Eval(ctx context.Context, _ string, _ []string, _ ...any) *goredis.Cmd {
	if f.evalResult != nil {
		return f.evalResult
	}
	cmd := goredis.NewCmd(ctx, "EVAL")
	cmd.SetErr(errors.New("Eval not stubbed"))
	return cmd
}

func (f *fakeRediser) EvalSha(ctx context.Context, _ string, _ []string, _ ...any) *goredis.Cmd {
	if f.evalShaResult != nil {
		return f.evalShaResult
	}
	cmd := goredis.NewCmd(ctx, "EVALSHA")
	cmd.SetErr(errors.New("EvalSha not stubbed"))
	return cmd
}

func (f *fakeRediser) ScriptExists(ctx context.Context, _ ...string) *goredis.BoolSliceCmd {
	cmd := goredis.NewBoolSliceCmd(ctx, "SCRIPT EXISTS")
	cmd.SetVal([]bool{true})
	return cmd
}

func (f *fakeRediser) ScriptLoad(ctx context.Context, _ string) *goredis.StringCmd {
	cmd := goredis.NewStringCmd(ctx, "SCRIPT LOAD")
	cmd.SetVal("sha")
	return cmd
}

func (f *fakeRediser) Del(ctx context.Context, _ ...string) *goredis.IntCmd {
	if f.delResult != nil {
		return f.delResult
	}
	cmd := goredis.NewIntCmd(ctx, "DEL")
	cmd.SetVal(0)
	return cmd
}

func (f *fakeRediser) EvalRO(ctx context.Context, s string, k []string, a ...any) *goredis.Cmd {
	return f.Eval(ctx, s, k, a...)
}

func (f *fakeRediser) EvalShaRO(ctx context.Context, s string, k []string, a ...any) *goredis.Cmd {
	return f.EvalSha(ctx, s, k, a...)
}

func evalShaCmd(values ...any) *goredis.Cmd {
	cmd := goredis.NewCmd(context.Background(), "EVALSHA")
	cmd.SetVal(values)
	return cmd
}

func evalShaErr(err error) *goredis.Cmd {
	cmd := goredis.NewCmd(context.Background(), "EVALSHA")
	cmd.SetErr(err)
	return cmd
}

func TestCheck_RejectsZeroLimit(t *testing.T) {
	t.Parallel()
	lim := redisadapter.New(&fakeRediser{})
	_, err := lim.Check(context.Background(), "k", ratelimit.Limit{})
	if !errors.Is(err, ratelimit.ErrInvalidLimit) {
		t.Fatalf("zero Limit error = %v, want ErrInvalidLimit", err)
	}
}

func TestCheck_AllowedMapsResetAfter(t *testing.T) {
	t.Parallel()
	// redis_rate Lua returns []any{allowed, remaining, retry_after, reset_after}
	// where the floats are encoded as decimal strings.
	res := evalShaCmd(int64(1), int64(4), "0", "0.500")
	lim := redisadapter.New(&fakeRediser{evalShaResult: res})

	dec, err := lim.Check(context.Background(), "k", ratelimit.Limit{Window: time.Second, Max: 5})
	if err != nil {
		t.Fatalf("Check err: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("Allowed = false, want true")
	}
	if dec.Remaining != 4 {
		t.Fatalf("Remaining = %d, want 4", dec.Remaining)
	}
	// allowed → Retry should mirror redis_rate's ResetAfter (500 ms)
	if dec.Retry != 500*time.Millisecond {
		t.Fatalf("Retry = %v, want 500ms", dec.Retry)
	}
}

func TestCheck_DeniedMapsRetryAfter(t *testing.T) {
	t.Parallel()
	res := evalShaCmd(int64(0), int64(0), "1.250", "1.500")
	lim := redisadapter.New(&fakeRediser{evalShaResult: res})

	dec, err := lim.Check(context.Background(), "k", ratelimit.Limit{Window: time.Second, Max: 5})
	if err != nil {
		t.Fatalf("Check err: %v", err)
	}
	if dec.Allowed {
		t.Fatal("Allowed = true, want false")
	}
	if dec.Remaining != 0 {
		t.Fatalf("Remaining = %d, want 0", dec.Remaining)
	}
	if dec.Retry != 1250*time.Millisecond {
		t.Fatalf("Retry = %v, want 1.25s", dec.Retry)
	}
}

func TestCheck_WrapsBackendErrorAsUnavailable(t *testing.T) {
	t.Parallel()
	infra := errors.New("connection refused")
	lim := redisadapter.New(&fakeRediser{
		// EvalSha fails; redis_rate falls back to Eval, which we also fail.
		evalShaResult: evalShaErr(infra),
		evalResult:    evalShaErr(infra),
	})
	_, err := lim.Check(context.Background(), "k", ratelimit.Limit{Window: time.Second, Max: 5})
	if err == nil {
		t.Fatal("Check returned nil error on backend failure")
	}
	if !errors.Is(err, ratelimit.ErrUnavailable) {
		t.Fatalf("error = %v, want wrapped ErrUnavailable", err)
	}
	if !errors.Is(err, infra) {
		t.Fatalf("error = %v, want wrapping the original infra error", err)
	}
}

func TestImplementsLimiter(t *testing.T) {
	t.Parallel()
	var _ ratelimit.Limiter = (*redisadapter.Limiter)(nil)
}
