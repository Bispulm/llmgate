package redisclient_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Bispulm/llmgate/internal/redisclient"
	"github.com/Bispulm/llmgate/internal/testenv"
)

func TestACallEndsAtItsDeadlineEvenIfRedisHangs(t *testing.T) {
	client := redisclient.FromOptions(&redis.Options{Addr: testenv.SilentRedis(t)})
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := client.Get(ctx, "k").Err(); err == nil {
		t.Fatal("no error from a hung Redis")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("took %v", elapsed)
	}
}

func TestAMalformedURLIsRefusedWithoutEchoingIt(t *testing.T) {
	_, err := redisclient.New("redis://user:secret@host:notaport/0")
	if err == nil {
		t.Fatal("no error")
	}
	if got := err.Error(); len(got) == 0 || contains(got, "secret") {
		t.Fatalf("err = %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
