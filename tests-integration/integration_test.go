package integration

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	fdpass "github.com/cgwalters/jsonrpc-fdpass-go"
)

const testServerBinary = "target/debug/test-server"

func TestIntegrationWithRustServer(t *testing.T) {
	// Check if the test server binary exists
	binaryPath := filepath.Join(".", testServerBinary)
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		t.Skipf("Test server binary not found at %s. Run 'just build-integration-server' to build it.", binaryPath)
	}

	// Create a temporary directory for the socket
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	// Start the Rust test server
	cmd := exec.Command(binaryPath, socketPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start test server: %v", err)
	}

	// Ensure we clean up the server process
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	// Wait for the server to start and create the socket
	var conn *net.UnixConn
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		var err error
		conn, err = net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
		if err == nil {
			break
		}
		if i == 49 {
			t.Fatalf("Failed to connect to server after 5 seconds: %v", err)
		}
	}
	defer conn.Close()

	sender := fdpass.NewSender(conn)
	receiver := fdpass.NewReceiver(conn)
	defer receiver.Close()

	// Run the test scenarios
	t.Run("echo", func(t *testing.T) {
		testEcho(t, sender, receiver)
	})

	t.Run("read_fd", func(t *testing.T) {
		testReadFD(t, sender, receiver)
	})

	t.Run("create_pipe", func(t *testing.T) {
		testCreatePipe(t, sender, receiver)
	})

	// Send shutdown notification to cleanly stop the server
	shutdownNotif := fdpass.NewNotification("shutdown", nil)
	if err := sender.Send(&fdpass.MessageWithFds{Message: shutdownNotif}); err != nil {
		t.Logf("Warning: failed to send shutdown notification: %v", err)
	}
}

func testEcho(t *testing.T, sender *fdpass.Sender, receiver *fdpass.Receiver) {
	// Send echo request
	req := fdpass.NewRequest("echo", map[string]interface{}{"message": "hello"}, 1)
	if err := sender.Send(&fdpass.MessageWithFds{Message: req}); err != nil {
		t.Fatalf("Failed to send echo request: %v", err)
	}

	// Receive response
	resp, err := receiver.Receive()
	if err != nil {
		t.Fatalf("Failed to receive echo response: %v", err)
	}

	response, ok := resp.Message.(*fdpass.Response)
	if !ok {
		t.Fatalf("Expected Response, got %T", resp.Message)
	}

	if response.Error != nil {
		t.Fatalf("Unexpected error response: %v", response.Error)
	}

	// Verify the echoed message
	result, ok := response.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("Expected result to be a map, got %T", response.Result)
	}

	message, ok := result["message"].(string)
	if !ok || message != "hello" {
		t.Errorf("Expected message 'hello', got %v", result["message"])
	}
}

func testReadFD(t *testing.T, sender *fdpass.Sender, receiver *fdpass.Receiver) {
	// Create a pipe
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatalf("Failed to create pipe: %v", err)
	}

	// Write test data to the pipe
	testData := "test data from go"
	go func() {
		pipeW.WriteString(testData)
		pipeW.Close()
	}()

	// Send read_fd request with the pipe's read end
	req := fdpass.NewRequest("read_fd", nil, 2)
	if err := sender.Send(&fdpass.MessageWithFds{
		Message:         req,
		FileDescriptors: []*os.File{pipeR},
	}); err != nil {
		pipeR.Close()
		t.Fatalf("Failed to send read_fd request: %v", err)
	}
	pipeR.Close() // Close our copy after sending

	// Receive response
	resp, err := receiver.Receive()
	if err != nil {
		t.Fatalf("Failed to receive read_fd response: %v", err)
	}

	response, ok := resp.Message.(*fdpass.Response)
	if !ok {
		t.Fatalf("Expected Response, got %T", resp.Message)
	}

	if response.Error != nil {
		t.Fatalf("Unexpected error response: %v", response.Error)
	}

	// Verify the server read the content correctly
	// The Rust server returns the content directly as a string
	content, ok := response.Result.(string)
	if !ok {
		t.Fatalf("Expected result to be a string, got %T", response.Result)
	}

	if content != testData {
		t.Errorf("Expected content '%s', got '%s'", testData, content)
	}
}

func testCreatePipe(t *testing.T, sender *fdpass.Sender, receiver *fdpass.Receiver) {
	// Send create_pipe request
	req := fdpass.NewRequest("create_pipe", nil, 3)
	if err := sender.Send(&fdpass.MessageWithFds{Message: req}); err != nil {
		t.Fatalf("Failed to send create_pipe request: %v", err)
	}

	// Receive response
	resp, err := receiver.Receive()
	if err != nil {
		t.Fatalf("Failed to receive create_pipe response: %v", err)
	}

	response, ok := resp.Message.(*fdpass.Response)
	if !ok {
		t.Fatalf("Expected Response, got %T", resp.Message)
	}

	if response.Error != nil {
		t.Fatalf("Unexpected error response: %v", response.Error)
	}

	// Verify we received an FD
	if len(resp.FileDescriptors) != 1 {
		t.Fatalf("Expected 1 FD, got %d", len(resp.FileDescriptors))
	}

	// Read from the received FD
	fd := resp.FileDescriptors[0]
	defer fd.Close()

	data, err := io.ReadAll(fd)
	if err != nil {
		t.Fatalf("Failed to read from received FD: %v", err)
	}

	expectedContent := "hello from rust"
	if string(data) != expectedContent {
		t.Errorf("Expected content '%s', got '%s'", expectedContent, string(data))
	}
}
