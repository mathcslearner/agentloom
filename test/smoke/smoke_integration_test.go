//go:build integration

// Package smoke holds minimal connectivity checks that prove the
// integration-test environment (local compose stack or CI service
// containers) is wired correctly, so a broken service definition is caught
// here rather than by whichever real suite happens to hit it first.
package smoke

import (
	"bufio"
	"net"
	"os"
	"testing"
	"time"
)

// redisAddr mirrors the storetest DSN convention: an AGENTLOOM_TEST_* env
// override with the compose-stack default. Real Redis suites (go-redis, M3)
// will grow their own harness; this stays a bare socket check on purpose.
func redisAddr() string {
	if v, ok := os.LookupEnv("AGENTLOOM_TEST_REDIS_ADDR"); ok && v != "" {
		return v
	}
	return "localhost:6379"
}

func TestRedisReachable(t *testing.T) {
	t.Parallel()
	conn, err := net.DialTimeout("tcp", redisAddr(), 5*time.Second)
	if err != nil {
		t.Fatalf("cannot reach Redis at %s (is the dev stack running? try `make up`): %v", redisAddr(), err)
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup in tests

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("setting deadline: %v", err)
	}
	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		t.Fatalf("writing PING: %v", err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("reading PING reply: %v", err)
	}
	if reply != "+PONG\r\n" {
		t.Fatalf("PING reply = %q, want +PONG", reply)
	}
}
