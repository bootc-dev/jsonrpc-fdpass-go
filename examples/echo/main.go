// Echo is a simple demonstration of JSON-RPC with FD passing.
//
// Usage:
//
//	go run main.go server /tmp/echo.sock
//	go run main.go client /tmp/echo.sock
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"

	fdpass "github.com/bootc-dev/jsonrpc-fdpass-go"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <server|client> <socket-path>\n", os.Args[0])
		os.Exit(1)
	}

	mode := os.Args[1]
	socketPath := os.Args[2]

	switch mode {
	case "server":
		runServer(socketPath)
	case "client":
		runClient(socketPath)
	default:
		fmt.Fprintf(os.Stderr, "Unknown mode: %s (use 'server' or 'client')\n", mode)
		os.Exit(1)
	}
}

func runServer(socketPath string) {
	// Remove existing socket
	os.Remove(socketPath)

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to listen: %v\n", err)
		os.Exit(1)
	}
	defer listener.Close()
	defer os.Remove(socketPath)

	fmt.Printf("Server listening on %s\n", socketPath)

	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Accept error: %v\n", err)
			continue
		}

		go handleConnection(conn)
	}
}

func handleConnection(conn *net.UnixConn) {
	defer conn.Close()

	receiver := fdpass.NewReceiver(conn)
	sender := fdpass.NewSender(conn)
	defer receiver.Close()

	fmt.Println("Client connected")

	for {
		msg, err := receiver.Receive()
		if err != nil {
			if err == fdpass.ErrConnectionClosed {
				fmt.Println("Client disconnected")
			} else {
				fmt.Fprintf(os.Stderr, "Receive error: %v\n", err)
			}
			return
		}

		switch m := msg.Message.(type) {
		case *fdpass.Request:
			handleRequest(sender, m, msg.FileDescriptors)
		case *fdpass.Notification:
			handleNotification(m, msg.FileDescriptors)
		default:
			fmt.Printf("Received unexpected message type: %T\n", msg.Message)
		}
	}
}

func handleRequest(sender *fdpass.Sender, req *fdpass.Request, fds []*os.File) {
	fmt.Printf("Request: method=%s id=%v fds=%d\n", req.Method, req.ID, len(fds))

	var result interface{}
	var respFDs []*os.File

	switch req.Method {
	case "echo":
		// Echo back the params
		result = req.Params

	case "readFile":
		// Read content from the passed FD
		if len(fds) > 0 {
			data, err := io.ReadAll(fds[0])
			fds[0].Close()
			if err != nil {
				sendError(sender, req.ID, -32000, err.Error())
				return
			}
			result = string(data)
		} else {
			sendError(sender, req.ID, -32602, "No file descriptor provided")
			return
		}

	case "createPipe":
		// Create a pipe and return the read end
		r, w, err := os.Pipe()
		if err != nil {
			sendError(sender, req.ID, -32000, err.Error())
			return
		}
		// Write some data and close write end
		go func() {
			w.WriteString("Data from server pipe")
			w.Close()
		}()
		result = map[string]interface{}{
			"message": "pipe created",
		}
		respFDs = []*os.File{r}

	default:
		sendError(sender, req.ID, -32601, "Method not found")
		return
	}

	resp := fdpass.NewResponse(result, req.ID)
	err := sender.Send(&fdpass.MessageWithFds{
		Message:         resp,
		FileDescriptors: respFDs,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to send response: %v\n", err)
	}
}

func handleNotification(notif *fdpass.Notification, fds []*os.File) {
	fmt.Printf("Notification: method=%s fds=%d\n", notif.Method, len(fds))

	// Close any received FDs since notifications don't get responses
	for _, fd := range fds {
		fd.Close()
	}
}

func sendError(sender *fdpass.Sender, id interface{}, code int, message string) {
	resp := fdpass.NewErrorResponse(&fdpass.Error{
		Code:    code,
		Message: message,
	}, id)
	sender.Send(&fdpass.MessageWithFds{Message: resp})
}

func runClient(socketPath string) {
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	sender := fdpass.NewSender(conn)
	receiver := fdpass.NewReceiver(conn)
	defer receiver.Close()

	fmt.Println("Connected to server")

	// Test 1: Simple echo
	fmt.Println("\n--- Test 1: Echo ---")
	req1 := fdpass.NewRequest("echo", map[string]interface{}{"hello": "world"}, 1)
	if err := sender.Send(&fdpass.MessageWithFds{Message: req1}); err != nil {
		fmt.Fprintf(os.Stderr, "Send error: %v\n", err)
		return
	}

	resp1, err := receiver.Receive()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Receive error: %v\n", err)
		return
	}
	printResponse(resp1)

	// Test 2: Send a file descriptor
	fmt.Println("\n--- Test 2: Read File via FD ---")
	r, w, _ := os.Pipe()
	go func() {
		w.WriteString("Hello from client pipe!")
		w.Close()
	}()

	req2 := fdpass.NewRequest("readFile", map[string]interface{}{
		"message": "read from the attached fd",
	}, 2)
	if err := sender.Send(&fdpass.MessageWithFds{
		Message:         req2,
		FileDescriptors: []*os.File{r},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Send error: %v\n", err)
		return
	}

	resp2, err := receiver.Receive()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Receive error: %v\n", err)
		return
	}
	printResponse(resp2)

	// Test 3: Receive a file descriptor from server
	fmt.Println("\n--- Test 3: Create Pipe (receive FD) ---")
	req3 := fdpass.NewRequest("createPipe", nil, 3)
	if err := sender.Send(&fdpass.MessageWithFds{Message: req3}); err != nil {
		fmt.Fprintf(os.Stderr, "Send error: %v\n", err)
		return
	}

	resp3, err := receiver.Receive()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Receive error: %v\n", err)
		return
	}
	printResponse(resp3)

	// Read from the received FD
	if len(resp3.FileDescriptors) > 0 {
		data, _ := io.ReadAll(resp3.FileDescriptors[0])
		resp3.FileDescriptors[0].Close()
		fmt.Printf("Read from server pipe: %s\n", string(data))
	}

	fmt.Println("\nAll tests completed!")
}

func printResponse(msg *fdpass.MessageWithFds) {
	resp, ok := msg.Message.(*fdpass.Response)
	if !ok {
		fmt.Printf("Unexpected message type: %T\n", msg.Message)
		return
	}

	if resp.Error != nil {
		fmt.Printf("Error: code=%d message=%s\n", resp.Error.Code, resp.Error.Message)
	} else {
		resultJSON, _ := json.MarshalIndent(resp.Result, "", "  ")
		fmt.Printf("Result: %s\n", string(resultJSON))
	}
	fmt.Printf("FDs received: %d\n", len(msg.FileDescriptors))
}
