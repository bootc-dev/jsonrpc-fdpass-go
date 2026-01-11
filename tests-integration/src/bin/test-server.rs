//! A simple JSON-RPC test server for cross-language integration testing.
//!
//! This server implements basic methods for testing the jsonrpc-fdpass protocol
//! between Rust and Go implementations.

use std::io::Read;
use std::os::unix::io::OwnedFd;

use jsonrpc_fdpass::{JsonRpcMessage, JsonRpcResponse, MessageWithFds, UnixSocketTransport};
use serde_json::Value;
use tokio::net::UnixListener;
use tracing::{debug, error, info};

#[tokio::main]
async fn main() -> jsonrpc_fdpass::Result<()> {
    // Initialize tracing
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::from_default_env()
                .add_directive(tracing::Level::DEBUG.into()),
        )
        .init();

    let args: Vec<String> = std::env::args().collect();
    if args.len() != 2 {
        eprintln!("Usage: {} <socket-path>", args[0]);
        std::process::exit(1);
    }

    let socket_path = &args[1];

    // Remove existing socket if present
    let _ = std::fs::remove_file(socket_path);

    info!("Starting test server on {}", socket_path);

    let listener = UnixListener::bind(socket_path)?;
    info!("Listening for connections");

    // Accept one connection for testing
    let (stream, _) = listener.accept().await?;
    info!("Client connected");

    let transport = UnixSocketTransport::new(stream);
    let (mut sender, mut receiver) = transport.split();

    let mut shutdown = false;

    loop {
        if shutdown {
            info!("Shutdown requested, exiting");
            break;
        }

        match receiver.receive().await {
            Ok(message_with_fds) => {
                match &message_with_fds.message {
                    JsonRpcMessage::Request(request) => {
                        let id = request.id.clone();
                        let method = request.method.as_str();
                        let params = request.params.clone();
                        let fds = message_with_fds.file_descriptors;

                        debug!("Received request: method={}, params={:?}", method, params);

                        let response = match method {
                            "echo" => handle_echo(params, id),
                            "read_fd" => handle_read_fd(params, fds, id),
                            "create_pipe" => handle_create_pipe(id),
                            _ => {
                                let error = jsonrpc_fdpass::JsonRpcError::owned(
                                    jsonrpc_fdpass::METHOD_NOT_FOUND_CODE,
                                    format!("Method not found: {}", method),
                                    None::<Value>,
                                );
                                MessageWithFds::new(
                                    JsonRpcMessage::Response(JsonRpcResponse::error(error, id)),
                                    Vec::new(),
                                )
                            }
                        };

                        sender.send(response).await?;
                    }
                    JsonRpcMessage::Notification(notification) => {
                        debug!("Received notification: {}", notification.method);

                        if notification.method == "shutdown" {
                            info!("Received shutdown notification");
                            shutdown = true;
                        }
                    }
                    JsonRpcMessage::Response(_) => {
                        debug!("Received unexpected response");
                    }
                }
            }
            Err(jsonrpc_fdpass::Error::ConnectionClosed) => {
                info!("Connection closed");
                break;
            }
            Err(e) => {
                error!("Error receiving message: {}", e);
                break;
            }
        }
    }

    // Clean up socket
    let _ = std::fs::remove_file(socket_path);
    info!("Server shutdown complete");

    Ok(())
}

/// Handle the `echo` method: returns params back as the result.
fn handle_echo(params: Option<Value>, id: Value) -> MessageWithFds {
    let result = params.unwrap_or(Value::Null);
    let response = JsonRpcResponse::success(result, id);
    MessageWithFds::new(JsonRpcMessage::Response(response), Vec::new())
}

/// Handle the `read_fd` method: reads content from an attached file descriptor.
fn handle_read_fd(
    _params: Option<Value>,
    mut fds: Vec<OwnedFd>,
    id: Value,
) -> MessageWithFds {
    if fds.is_empty() {
        let error = jsonrpc_fdpass::JsonRpcError::owned(
            jsonrpc_fdpass::INVALID_PARAMS_CODE,
            "No file descriptor provided".to_string(),
            None::<Value>,
        );
        return MessageWithFds::new(
            JsonRpcMessage::Response(JsonRpcResponse::error(error, id)),
            Vec::new(),
        );
    }

    let fd = fds.remove(0);
    // Close any extra FDs
    drop(fds);

    // Convert OwnedFd to File for reading
    let mut file = std::fs::File::from(fd);

    let mut contents = String::new();
    match file.read_to_string(&mut contents) {
        Ok(_) => {
            let response = JsonRpcResponse::success(Value::String(contents), id);
            MessageWithFds::new(JsonRpcMessage::Response(response), Vec::new())
        }
        Err(e) => {
            let error = jsonrpc_fdpass::JsonRpcError::owned(
                jsonrpc_fdpass::INTERNAL_ERROR_CODE,
                format!("Failed to read from fd: {}", e),
                None::<Value>,
            );
            MessageWithFds::new(
                JsonRpcMessage::Response(JsonRpcResponse::error(error, id)),
                Vec::new(),
            )
        }
    }
}

/// Handle the `create_pipe` method: creates a pipe, writes "hello from rust" to it,
/// and returns the read end.
fn handle_create_pipe(id: Value) -> MessageWithFds {
    use std::io::Write;

    // Create a pipe
    let (read_fd, write_fd) = rustix::pipe::pipe().unwrap();

    // Write to the write end
    let mut write_file = std::fs::File::from(write_fd);
    if let Err(e) = write_file.write_all(b"hello from rust") {
        let error = jsonrpc_fdpass::JsonRpcError::owned(
            jsonrpc_fdpass::INTERNAL_ERROR_CODE,
            format!("Failed to write to pipe: {}", e),
            None::<Value>,
        );
        return MessageWithFds::new(
            JsonRpcMessage::Response(JsonRpcResponse::error(error, id)),
            Vec::new(),
        );
    }
    // Close the write end by dropping the file
    drop(write_file);

    // read_fd is already an OwnedFd from rustix
    let read_owned_fd: OwnedFd = read_fd;

    let response = JsonRpcResponse::success(Value::String("pipe created".to_string()), id);
    MessageWithFds::new(JsonRpcMessage::Response(response), vec![read_owned_fd])
}
