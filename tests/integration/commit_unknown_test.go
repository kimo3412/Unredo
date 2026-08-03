//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/girimi/unredo/internal/planner"
)

func TestCommitResponseLossCanBeVerifiedWithoutRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	mysqlBin := findMySQLBin(t)
	rootConn := openRoot(t, mysqlBin)
	defer rootConn.Close()
	ensureFullRowMetadata(t, rootConn)
	execConn := openExecutor(t, mysqlBin)
	defer execConn.Close()

	marker := fmt.Sprintf("unknown-%d", time.Now().UnixNano()%10000000)
	result, err := execConn.Exec(
		"INSERT INTO unredo_shop.orders (user_id, status, amount) VALUES (?, ?, ?)",
		995001, marker, "7.50",
	)
	if err != nil {
		t.Fatalf("seed commit-unknown row: %v", err)
	}
	rowID, _ := result.LastInsertId()
	t.Cleanup(func() { _, _ = execConn.Exec("DELETE FROM unredo_shop.orders WHERE id = ?", rowID) })

	gtid, err := latestGTID(rootConn)
	if err != nil {
		t.Fatal(err)
	}
	planPath := createPlanForGTID(t, rootConn, gtid)
	plan, err := planner.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}

	proxy := newCommitResponseDropProxy(t, "127.0.0.1:3306")
	configPath := writeIntegrationConfig(t, proxy.Address(), 20)
	applyOutput, applyErr := runCLIWithConfig(t, configPath,
		"plan", "apply", planPath,
		"--non-interactive",
		"--confirm-sha", planner.ShortDigest(plan.Digest),
		"--operator", "commit-unknown-test",
		"--log-level", "error",
	)
	if applyErr == nil {
		t.Fatalf("response loss unexpectedly reported success:\n%s", applyOutput)
	}
	if !strings.Contains(applyOutput, "COMMIT_UNKNOWN") || !strings.Contains(applyOutput, "retry:        FORBIDDEN") {
		t.Fatalf("missing commit-unknown recovery output:\n%s", applyOutput)
	}
	if !proxy.DroppedCommitResponse() {
		t.Fatalf("proxy did not drop the COMMIT response:\n%s", applyOutput)
	}
	actionID := outputField(t, applyOutput, "action_id")

	verifyOutput, verifyErr := runCLIWithConfig(t, configPath,
		"action", "verify",
		"--action-id", actionID,
		"--plan", planPath,
		"--wait", "0s",
	)
	if verifyErr != nil || !strings.Contains(verifyOutput, "status:      COMMITTED") {
		t.Fatalf("could not resolve response loss as COMMITTED: err=%v\n%s", verifyErr, verifyOutput)
	}

	var rowCount int
	if err := execConn.QueryRow("SELECT COUNT(*) FROM unredo_shop.orders WHERE id = ?", rowID).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 0 {
		t.Fatalf("committed compensation left row %d present", rowID)
	}

	// Even if an operator ignores the recovery message, a second apply must
	// not create another successful marker or repeat the compensation.
	retryOutput, retryErr := runCLIWithConfig(t, configPath,
		"plan", "apply", planPath,
		"--non-interactive",
		"--confirm-sha", planner.ShortDigest(plan.Digest),
		"--operator", "commit-unknown-test",
		"--log-level", "error",
	)
	if retryErr == nil {
		t.Fatalf("commit-unknown plan unexpectedly applied twice:\n%s", retryOutput)
	}
	planID, err := ulid.ParseStrict(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	var markerCount int
	if err := rootConn.QueryRow("SELECT COUNT(*) FROM unredo_meta.action_markers WHERE plan_id = ?", planID[:]).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if markerCount != 1 {
		t.Fatalf("plan has %d committed markers; want exactly one", markerCount)
	}
}

func writeIntegrationConfig(t *testing.T, targetAddress string, maxActionDepth int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unredo-proxy.yaml")
	content := fmt.Sprintf(`version: 1
profiles:
  fault:
    backend: mysql
    source:
      mode: replication
      address: 127.0.0.1:3306
      user: unredo_reader
      password_env: UNREDO_READER_PASSWORD
      server_id: 100002
    target:
      address: %s
      user: unredo_executor
      password_env: UNREDO_EXECUTOR_PASSWORD
    policy:
      require_gtid: true
      require_full_row_image: true
      require_primary_key: true
      max_transaction_rows: 1000
      max_transaction_bytes: 67108864
      max_plan_bytes: 134217728
      max_action_depth: %d
      lock_wait_timeout: 5s
`, targetAddress, maxActionDepth)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write proxy config: %v", err)
	}
	return path
}

func runCLIWithConfig(t *testing.T, configPath string, args ...string) (string, error) {
	t.Helper()
	repoRoot := repoRoot(t)
	base := []string{"--config", configPath, "--profile", "fault"}
	cmd := exec.Command(filepath.Join(repoRoot, "bin", "unredo.exe"), append(base, args...)...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"UNREDO_READER_PASSWORD="+readerPass,
		"UNREDO_EXECUTOR_PASSWORD="+executorPass,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// commitResponseDropProxy is a transparent MySQL packet relay. It forwards a
// COM_QUERY "COMMIT" to MySQL, consumes the first response packet, then closes
// the connection so the client cannot know whether the server committed.
type commitResponseDropProxy struct {
	listener net.Listener
	upstream string
	dropped  atomic.Bool
	once     sync.Once
}

func newCommitResponseDropProxy(t *testing.T, upstream string) *commitResponseDropProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for commit proxy: %v", err)
	}
	p := &commitResponseDropProxy{listener: listener, upstream: upstream}
	go p.serve()
	t.Cleanup(p.Close)
	return p
}

func (p *commitResponseDropProxy) Address() string { return p.listener.Addr().String() }

func (p *commitResponseDropProxy) DroppedCommitResponse() bool { return p.dropped.Load() }

func (p *commitResponseDropProxy) Close() {
	p.once.Do(func() {
		_ = p.listener.Close()
	})
}

func (p *commitResponseDropProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.handle(client)
	}
}

func (p *commitResponseDropProxy) handle(client net.Conn) {
	defer client.Close()
	server, err := net.DialTimeout("tcp", p.upstream, 5*time.Second)
	if err != nil {
		return
	}
	defer server.Close()

	var commitPending atomic.Bool
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			raw, payload, err := readMySQLPacket(client)
			if err != nil {
				return
			}
			if isCommitQuery(payload) {
				commitPending.Store(true)
			}
			if err := writeAll(server, raw); err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			raw, _, err := readMySQLPacket(server)
			if err != nil {
				return
			}
			if commitPending.Swap(false) && p.dropped.CompareAndSwap(false, true) {
				return
			}
			if err := writeAll(client, raw); err != nil {
				return
			}
		}
	}()

	<-done
	_ = client.Close()
	_ = server.Close()
	<-done
}

func readMySQLPacket(conn net.Conn) ([]byte, []byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, nil, err
	}
	length := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	if length > 64<<20 {
		return nil, nil, fmt.Errorf("mysql packet too large: %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, nil, err
	}
	raw := make([]byte, 0, len(header)+len(payload))
	raw = append(raw, header...)
	raw = append(raw, payload...)
	return raw, payload, nil
}

func isCommitQuery(payload []byte) bool {
	return len(payload) > 1 && payload[0] == 0x03 && strings.EqualFold(strings.TrimSpace(string(payload[1:])), "COMMIT")
}

func writeAll(conn net.Conn, payload []byte) error {
	_, err := io.Copy(conn, bytes.NewReader(payload))
	return err
}
