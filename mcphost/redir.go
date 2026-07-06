package mcphost

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
)

// RedirConfig configures the dumb redirector entrypoint. Subcommand and
// the env var names must match the Host that spawned it (they run in the
// same consumer binary).
type RedirConfig struct {
	// Subcommand is the primary argv token the redirector dispatches on.
	Subcommand string
	// Aliases are additional accepted argv tokens (e.g. a deprecated
	// legacy name).
	Aliases []string
	// EnvSocket / EnvToken are the env var names carrying the socket path
	// and token.
	EnvSocket string
	EnvToken  string
}

// MaybeRunRedir inspects os.Args: if os.Args[1] matches the configured
// subcommand or one of its aliases, it runs the redirector and returns
// handled=true with the run result. Otherwise it returns handled=false
// and the consumer continues normal startup. Call it early in main.
func MaybeRunRedir(cfg RedirConfig) (handled bool, err error) {
	if len(os.Args) <= 1 {
		return false, nil
	}
	arg := os.Args[1]
	if arg != cfg.Subcommand {
		match := false
		for _, a := range cfg.Aliases {
			if arg == a {
				match = true
				break
			}
		}
		if !match {
			return false, nil
		}
	}
	return true, RunRedir(cfg)
}

// RunRedir is the redirector entrypoint: a dumb pipe. It reads the socket
// path and token from the env the parent set, dials the socket, writes
// the preamble, and pumps stdin↔socket. It has no MCP knowledge.
func RunRedir(cfg RedirConfig) error {
	socket := os.Getenv(cfg.EnvSocket)
	token := os.Getenv(cfg.EnvToken)
	if socket == "" {
		return fmt.Errorf("mcphost redir: %s not set", cfg.EnvSocket)
	}
	return redirect(socket, token, os.Stdin, os.Stdout)
}

// redirect dials the unix socket and pumps the streams.
func redirect(socket, token string, in io.Reader, out io.Writer) error {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("dial %s: %w", socket, err)
	}
	defer conn.Close()
	return pump(conn, token, in, out)
}

// pump writes the {"token":...} preamble line, then io.Copy's in→conn
// and conn→out concurrently. It returns when the conn→out direction
// completes (the server closed its side), having half-closed the write
// side once stdin hits EOF so the server's MCP loop terminates. This
// relies on the invariant that the server always closes the connection
// after it sees stdin EOF (runMCP returns on read EOF and the handler
// defers conn.Close); otherwise the <-done wait would block forever.
func pump(conn net.Conn, token string, in io.Reader, out io.Writer) error {
	pre, _ := json.Marshal(struct {
		Token string `json:"token"`
	}{Token: token})
	if _, err := conn.Write(append(pre, '\n')); err != nil {
		return fmt.Errorf("write preamble: %w", err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(conn, in)
		halfCloseWrite(conn) // signal EOF to the server's MCP loop
	}()
	go func() {
		_, _ = io.Copy(out, conn)
		close(done) // server closed its side: redirection is finished
	}()
	<-done
	return nil
}

// halfCloseWrite closes only the write half of conn if supported (real
// unix sockets do; net.Pipe does not), so the server sees EOF on stdin
// while we keep reading its response.
func halfCloseWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
