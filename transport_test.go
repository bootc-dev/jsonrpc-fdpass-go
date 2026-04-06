package fdpass

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestBasicMessageSerialization(t *testing.T) {
	req := NewRequest("test_method", map[string]interface{}{"param": "value"}, 1)

	msg := &MessageWithFds{
		Message:         req,
		FileDescriptors: nil,
	}

	if msg.Message == nil {
		t.Fatal("message should not be nil")
	}
}

func TestGetFDCount(t *testing.T) {
	tests := []struct {
		name     string
		value    map[string]interface{}
		expected int
	}{
		{
			name:     "no fds field",
			value:    map[string]interface{}{"method": "test"},
			expected: 0,
		},
		{
			name:     "fds field with 0",
			value:    map[string]interface{}{"method": "test", "fds": float64(0)},
			expected: 0,
		},
		{
			name:     "fds field with 1",
			value:    map[string]interface{}{"method": "test", "fds": float64(1)},
			expected: 1,
		},
		{
			name:     "fds field with 5",
			value:    map[string]interface{}{"method": "test", "fds": float64(5)},
			expected: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count := GetFDCount(tt.value)
			if count != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, count)
			}
		})
	}
}

func TestSetGetFDs(t *testing.T) {
	t.Run("Request", func(t *testing.T) {
		req := NewRequest("test", nil, 1)
		if req.GetFDs() != 0 {
			t.Errorf("expected 0, got %d", req.GetFDs())
		}
		req.SetFDs(2)
		if req.GetFDs() != 2 {
			t.Errorf("expected 2, got %d", req.GetFDs())
		}
		req.SetFDs(0)
		if req.GetFDs() != 0 {
			t.Errorf("expected 0 after setting to 0, got %d", req.GetFDs())
		}
	})

	t.Run("Response", func(t *testing.T) {
		resp := NewResponse("result", 1)
		if resp.GetFDs() != 0 {
			t.Errorf("expected 0, got %d", resp.GetFDs())
		}
		resp.SetFDs(3)
		if resp.GetFDs() != 3 {
			t.Errorf("expected 3, got %d", resp.GetFDs())
		}
	})

	t.Run("Notification", func(t *testing.T) {
		notif := NewNotification("test", nil)
		if notif.GetFDs() != 0 {
			t.Errorf("expected 0, got %d", notif.GetFDs())
		}
		notif.SetFDs(1)
		if notif.GetFDs() != 1 {
			t.Errorf("expected 1, got %d", notif.GetFDs())
		}
	})
}

func TestParseMessage(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		wantType string
	}{
		{
			name:     "request",
			json:     `{"jsonrpc": "2.0", "method": "test", "params": {}, "id": 1}`,
			wantType: "*fdpass.Request",
		},
		{
			name:     "response with result",
			json:     `{"jsonrpc": "2.0", "result": "success", "id": 1}`,
			wantType: "*fdpass.Response",
		},
		{
			name:     "response with error",
			json:     `{"jsonrpc": "2.0", "error": {"code": -32600, "message": "Invalid"}, "id": 1}`,
			wantType: "*fdpass.Response",
		},
		{
			name:     "notification",
			json:     `{"jsonrpc": "2.0", "method": "notify", "params": {}}`,
			wantType: "*fdpass.Notification",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := ParseMessage([]byte(tt.json))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			gotType := ""
			switch msg.(type) {
			case *Request:
				gotType = "*fdpass.Request"
			case *Response:
				gotType = "*fdpass.Response"
			case *Notification:
				gotType = "*fdpass.Notification"
			}

			if gotType != tt.wantType {
				t.Errorf("expected type %s, got %s", tt.wantType, gotType)
			}
		})
	}
}

func TestSocketpairCommunication(t *testing.T) {
	// Create a socketpair
	fds, err := createSocketPair()
	if err != nil {
		t.Fatalf("failed to create socketpair: %v", err)
	}
	defer fds[0].Close()
	defer fds[1].Close()

	// Create sender and receiver
	sender := NewSender(fds[0])
	receiver := NewReceiver(fds[1])
	defer receiver.Close()

	// Send a notification without FDs
	notif := NewNotification("test_method", map[string]interface{}{"key": "value"})
	msg := &MessageWithFds{
		Message:         notif,
		FileDescriptors: nil,
	}

	// Send in goroutine
	done := make(chan error, 1)
	go func() {
		done <- sender.Send(msg)
	}()

	// Receive
	received, err := receiver.Receive()
	if err != nil {
		t.Fatalf("failed to receive: %v", err)
	}

	// Wait for send to complete
	if err := <-done; err != nil {
		t.Fatalf("failed to send: %v", err)
	}

	// Verify
	notifReceived, ok := received.Message.(*Notification)
	if !ok {
		t.Fatalf("expected Notification, got %T", received.Message)
	}
	if notifReceived.Method != "test_method" {
		t.Errorf("expected method 'test_method', got '%s'", notifReceived.Method)
	}
	if len(received.FileDescriptors) != 0 {
		t.Errorf("expected 0 FDs, got %d", len(received.FileDescriptors))
	}
}

func TestSocketpairWithFDs(t *testing.T) {
	// Create a socketpair
	fds, err := createSocketPair()
	if err != nil {
		t.Fatalf("failed to create socketpair: %v", err)
	}
	defer fds[0].Close()
	defer fds[1].Close()

	// Create test pipes
	pipeR1, pipeW1, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe 1: %v", err)
	}
	defer pipeR1.Close()

	pipeR2, pipeW2, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe 2: %v", err)
	}
	defer pipeR2.Close()

	// Write test data to pipes
	testData1 := "Hello from pipe 1"
	testData2 := "Hello from pipe 2"
	go func() {
		pipeW1.WriteString(testData1)
		pipeW1.Close()
		pipeW2.WriteString(testData2)
		pipeW2.Close()
	}()

	// Create sender and receiver
	sender := NewSender(fds[0])
	receiver := NewReceiver(fds[1])
	defer receiver.Close()

	// Send a notification with 2 FDs
	notif := NewNotification("read_files", map[string]interface{}{
		"message": "please read these files",
	})
	msg := &MessageWithFds{
		Message:         notif,
		FileDescriptors: []*os.File{pipeR1, pipeR2},
	}

	// Send in goroutine
	done := make(chan error, 1)
	go func() {
		done <- sender.Send(msg)
	}()

	// Receive
	received, err := receiver.Receive()
	if err != nil {
		t.Fatalf("failed to receive: %v", err)
	}

	// Wait for send to complete
	if err := <-done; err != nil {
		t.Fatalf("failed to send: %v", err)
	}

	// Verify message
	notifReceived, ok := received.Message.(*Notification)
	if !ok {
		t.Fatalf("expected Notification, got %T", received.Message)
	}
	if notifReceived.Method != "read_files" {
		t.Errorf("expected method 'read_files', got '%s'", notifReceived.Method)
	}

	// Verify FDs
	if len(received.FileDescriptors) != 2 {
		t.Fatalf("expected 2 FDs, got %d", len(received.FileDescriptors))
	}

	// Read from received FDs
	data1, err := io.ReadAll(received.FileDescriptors[0])
	if err != nil {
		t.Fatalf("failed to read from FD 0: %v", err)
	}
	received.FileDescriptors[0].Close()

	data2, err := io.ReadAll(received.FileDescriptors[1])
	if err != nil {
		t.Fatalf("failed to read from FD 1: %v", err)
	}
	received.FileDescriptors[1].Close()

	if string(data1) != testData1 {
		t.Errorf("expected '%s', got '%s'", testData1, string(data1))
	}
	if string(data2) != testData2 {
		t.Errorf("expected '%s', got '%s'", testData2, string(data2))
	}
}

func TestMultipleMessages(t *testing.T) {
	// Create a socketpair
	fds, err := createSocketPair()
	if err != nil {
		t.Fatalf("failed to create socketpair: %v", err)
	}
	defer fds[0].Close()
	defer fds[1].Close()

	sender := NewSender(fds[0])
	receiver := NewReceiver(fds[1])
	defer receiver.Close()

	// Send multiple messages
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 3; i++ {
			notif := NewNotification("test", map[string]interface{}{"index": i})
			msg := &MessageWithFds{Message: notif}
			if err := sender.Send(msg); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	// Receive all messages
	for i := 0; i < 3; i++ {
		received, err := receiver.Receive()
		if err != nil {
			t.Fatalf("failed to receive message %d: %v", i, err)
		}
		notif, ok := received.Message.(*Notification)
		if !ok {
			t.Fatalf("expected Notification for message %d", i)
		}
		if notif.Method != "test" {
			t.Errorf("expected method 'test' for message %d", i)
		}
	}

	if err := <-done; err != nil {
		t.Fatalf("sender error: %v", err)
	}
}

func TestMixedMessagesWithAndWithoutFDs(t *testing.T) {
	// Create a socketpair
	fds, err := createSocketPair()
	if err != nil {
		t.Fatalf("failed to create socketpair: %v", err)
	}
	defer fds[0].Close()
	defer fds[1].Close()

	sender := NewSender(fds[0])
	receiver := NewReceiver(fds[1])
	defer receiver.Close()

	done := make(chan error, 1)
	go func() {
		// Message 1: no FD
		notif1 := NewNotification("no_fd", nil)
		if err := sender.Send(&MessageWithFds{Message: notif1}); err != nil {
			done <- err
			return
		}

		// Message 2: with FD
		pipeR, pipeW, _ := os.Pipe()
		go func() {
			pipeW.WriteString("pipe data")
			pipeW.Close()
		}()
		notif2 := NewNotification("with_fd", map[string]interface{}{"message": "has file attached"})
		if err := sender.Send(&MessageWithFds{Message: notif2, FileDescriptors: []*os.File{pipeR}}); err != nil {
			done <- err
			return
		}

		// Message 3: no FD
		notif3 := NewNotification("no_fd_again", nil)
		if err := sender.Send(&MessageWithFds{Message: notif3}); err != nil {
			done <- err
			return
		}

		done <- nil
	}()

	// Receive message 1
	msg1, err := receiver.Receive()
	if err != nil {
		t.Fatalf("failed to receive message 1: %v", err)
	}
	if msg1.Message.(*Notification).Method != "no_fd" {
		t.Error("message 1 method mismatch")
	}
	if len(msg1.FileDescriptors) != 0 {
		t.Error("message 1 should have no FDs")
	}

	// Receive message 2
	msg2, err := receiver.Receive()
	if err != nil {
		t.Fatalf("failed to receive message 2: %v", err)
	}
	if msg2.Message.(*Notification).Method != "with_fd" {
		t.Error("message 2 method mismatch")
	}
	if len(msg2.FileDescriptors) != 1 {
		t.Fatalf("message 2 should have 1 FD, got %d", len(msg2.FileDescriptors))
	}

	data, _ := io.ReadAll(msg2.FileDescriptors[0])
	msg2.FileDescriptors[0].Close()
	if string(data) != "pipe data" {
		t.Errorf("expected 'pipe data', got '%s'", string(data))
	}

	// Receive message 3
	msg3, err := receiver.Receive()
	if err != nil {
		t.Fatalf("failed to receive message 3: %v", err)
	}
	if msg3.Message.(*Notification).Method != "no_fd_again" {
		t.Error("message 3 method mismatch")
	}
	if len(msg3.FileDescriptors) != 0 {
		t.Error("message 3 should have no FDs")
	}

	if err := <-done; err != nil {
		t.Fatalf("sender error: %v", err)
	}
}

func TestRequestResponse(t *testing.T) {
	fds, err := createSocketPair()
	if err != nil {
		t.Fatalf("failed to create socketpair: %v", err)
	}
	defer fds[0].Close()
	defer fds[1].Close()

	sender := NewSender(fds[0])
	receiver := NewReceiver(fds[1])
	defer receiver.Close()

	// Send request
	go func() {
		req := NewRequest("add", map[string]interface{}{"a": 1, "b": 2}, 42)
		sender.Send(&MessageWithFds{Message: req})
	}()

	// Receive request
	received, err := receiver.Receive()
	if err != nil {
		t.Fatalf("failed to receive: %v", err)
	}

	req, ok := received.Message.(*Request)
	if !ok {
		t.Fatalf("expected Request, got %T", received.Message)
	}
	if req.Method != "add" {
		t.Errorf("expected method 'add', got '%s'", req.Method)
	}
	if req.ID != float64(42) { // JSON numbers become float64
		t.Errorf("expected ID 42, got %v", req.ID)
	}
}

// testFDBatching is a helper that sends fdCount FDs in a single message with
// the sender's max-per-sendmsg set to maxPerSendmsg, then verifies that all
// FDs arrive correctly on the receiver side with the right data and ordering.
func testFDBatching(t *testing.T, fdCount, maxPerSendmsg int) {
	t.Helper()

	fds, err := createSocketPair()
	if err != nil {
		t.Fatalf("failed to create socketpair: %v", err)
	}
	defer fds[0].Close()
	defer fds[1].Close()

	// Create pipes as test FDs
	type pipePair struct {
		r *os.File
		w *os.File
	}
	pipes := make([]pipePair, fdCount)
	sendFDs := make([]*os.File, fdCount)
	for i := 0; i < fdCount; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("failed to create pipe %d: %v", i, err)
		}
		pipes[i] = pipePair{r, w}
		sendFDs[i] = r
	}

	// Write distinct data to each pipe so we can verify ordering
	go func() {
		for i, p := range pipes {
			fmt.Fprintf(p.w, "fd-data-%d", i)
			p.w.Close()
		}
	}()

	sender := NewSender(fds[0])
	sender.SetMaxFDsPerSendmsg(maxPerSendmsg)
	receiver := NewReceiver(fds[1])
	defer receiver.Close()

	notif := NewNotification("batch_test", map[string]interface{}{
		"count": fdCount,
	})
	msg := &MessageWithFds{
		Message:         notif,
		FileDescriptors: sendFDs,
	}

	done := make(chan error, 1)
	go func() {
		done <- sender.Send(msg)
	}()

	received, err := receiver.Receive()
	if err != nil {
		t.Fatalf("failed to receive (fdCount=%d, maxPerSendmsg=%d): %v",
			fdCount, maxPerSendmsg, err)
	}

	if err := <-done; err != nil {
		t.Fatalf("failed to send: %v", err)
	}

	// Verify message type
	notifReceived, ok := received.Message.(*Notification)
	if !ok {
		t.Fatalf("expected Notification, got %T", received.Message)
	}
	if notifReceived.Method != "batch_test" {
		t.Errorf("expected method 'batch_test', got '%s'", notifReceived.Method)
	}

	// Verify FD count
	if len(received.FileDescriptors) != fdCount {
		t.Fatalf("expected %d FDs, got %d", fdCount, len(received.FileDescriptors))
	}

	// Verify each FD has the correct data (ordering preserved)
	for i, f := range received.FileDescriptors {
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatalf("failed to read from FD %d: %v", i, err)
		}
		expected := fmt.Sprintf("fd-data-%d", i)
		if string(data) != expected {
			t.Errorf("FD %d: expected '%s', got '%s'", i, expected, string(data))
		}
	}

	// Clean up sender-side read ends
	for _, p := range pipes {
		p.r.Close()
	}
}

func TestFDBatchingSmallBatches(t *testing.T) {
	// Test with batch sizes of 1, 2, 3 to stress the batching logic.
	// Sending 10 FDs with batch size 1 means 10 separate sendmsg calls
	// (1 with JSON data + 9 with whitespace padding).
	for _, batchSize := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("batch_%d_fds_10", batchSize), func(t *testing.T) {
			testFDBatching(t, 10, batchSize)
		})
	}
}

func TestFDBatchingManyFDs(t *testing.T) {
	// Test sending many FDs with various batch sizes
	for _, tc := range []struct {
		fdCount        int
		maxPerSendmsg  int
	}{
		{20, 3},
		{50, 7},
		{100, 10},
	} {
		t.Run(fmt.Sprintf("fds_%d_batch_%d", tc.fdCount, tc.maxPerSendmsg), func(t *testing.T) {
			testFDBatching(t, tc.fdCount, tc.maxPerSendmsg)
		})
	}
}

func TestFDBatchingNoBatching(t *testing.T) {
	// When batch size >= fd count, no batching occurs (single sendmsg)
	testFDBatching(t, 5, 500)
}

func TestFDBatchingFollowedByNonFDMessage(t *testing.T) {
	// Verify that after a batched FD message, a subsequent message without
	// FDs is received correctly (no leftover whitespace confusion).
	fds, err := createSocketPair()
	if err != nil {
		t.Fatalf("failed to create socketpair: %v", err)
	}
	defer fds[0].Close()
	defer fds[1].Close()

	sender := NewSender(fds[0])
	sender.SetMaxFDsPerSendmsg(2) // Force batching
	receiver := NewReceiver(fds[1])
	defer receiver.Close()

	done := make(chan error, 1)
	go func() {
		// Message 1: 5 FDs, batched in groups of 2
		pipes := make([]*os.File, 5)
		for i := range pipes {
			r, w, _ := os.Pipe()
			go func(w *os.File) { w.Close() }(w)
			pipes[i] = r
		}
		notif1 := NewNotification("with_fds", nil)
		if err := sender.Send(&MessageWithFds{Message: notif1, FileDescriptors: pipes}); err != nil {
			done <- err
			return
		}

		// Message 2: no FDs
		notif2 := NewNotification("no_fds", nil)
		if err := sender.Send(&MessageWithFds{Message: notif2}); err != nil {
			done <- err
			return
		}

		done <- nil
	}()

	// Receive message 1 (batched FDs)
	msg1, err := receiver.Receive()
	if err != nil {
		t.Fatalf("failed to receive message 1: %v", err)
	}
	if msg1.Message.(*Notification).Method != "with_fds" {
		t.Error("message 1 method mismatch")
	}
	if len(msg1.FileDescriptors) != 5 {
		t.Fatalf("expected 5 FDs, got %d", len(msg1.FileDescriptors))
	}
	for _, f := range msg1.FileDescriptors {
		f.Close()
	}

	// Receive message 2 (no FDs)
	msg2, err := receiver.Receive()
	if err != nil {
		t.Fatalf("failed to receive message 2: %v", err)
	}
	if msg2.Message.(*Notification).Method != "no_fds" {
		t.Error("message 2 method mismatch")
	}
	if len(msg2.FileDescriptors) != 0 {
		t.Errorf("expected 0 FDs, got %d", len(msg2.FileDescriptors))
	}

	if err := <-done; err != nil {
		t.Fatalf("sender error: %v", err)
	}
}

func TestFDBatchingSingleFD(t *testing.T) {
	// Edge case: single FD with batch size 1 should work (no continuation needed)
	testFDBatching(t, 1, 1)
}

func TestFDBatchingExactBoundary(t *testing.T) {
	// FD count exactly equals batch size — no continuation sendmsg needed
	testFDBatching(t, 5, 5)
	// FD count is one more than batch size — exactly one continuation
	testFDBatching(t, 6, 5)
}

// createSocketPair creates a pair of connected Unix sockets.
func createSocketPair() ([2]*net.UnixConn, error) {
	// Create a temp directory for the socket
	tmpDir, err := os.MkdirTemp("", "fdpass-test")
	if err != nil {
		return [2]*net.UnixConn{}, err
	}
	socketPath := filepath.Join(tmpDir, "test.sock")

	// Clean up temp dir when done (will be cleaned up by OS on process exit anyway)
	defer os.RemoveAll(tmpDir)

	// Create listener
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return [2]*net.UnixConn{}, err
	}
	defer listener.Close()

	// Connect client in goroutine
	clientChan := make(chan *net.UnixConn, 1)
	errChan := make(chan error, 1)
	go func() {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
		if err != nil {
			errChan <- err
			return
		}
		clientChan <- conn
	}()

	// Accept server connection
	serverConn, err := listener.AcceptUnix()
	if err != nil {
		return [2]*net.UnixConn{}, err
	}

	// Get client connection
	select {
	case clientConn := <-clientChan:
		return [2]*net.UnixConn{clientConn, serverConn}, nil
	case err := <-errChan:
		serverConn.Close()
		return [2]*net.UnixConn{}, err
	}
}
