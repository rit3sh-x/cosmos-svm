//! The QUIC client svm-tpu's integration tests drive. Each mode prints one line
//! per outcome, and the Go test waits on the line it needs.
//!
//!   quic-client send ADDR FILE...      one connection, one stream per file
//!   quic-client oversized ADDR         a 4,097-byte stream, then the close code
//!   quic-client connections ADDR N     N connections opened back to back and
//!                                      held, each line "open", "closed CODE"
//!                                      or "no handshake"

use {
    quinn::{ConnectionError, Endpoint, EndpointConfig, TokioRuntime},
    solana_keypair::Keypair,
    solana_streamer::nonblocking::testing_utilities::get_client_config,
    std::{net::SocketAddr, sync::Arc, time::Duration},
};

/// One endpoint per connection, on its own port of every IPv4 interface, as
/// the CLI's client binds an IPv4 socket. A socket bound to 127.0.0.1 reaches
/// only a server on the same machine, and a multi-machine run's handshakes
/// time out.
async fn connect(addr: SocketAddr) -> (Endpoint, Result<quinn::Connection, ConnectionError>) {
    let socket = std::net::UdpSocket::bind("0.0.0.0:0").expect("bind");
    let mut endpoint = Endpoint::new(EndpointConfig::default(), None, socket, Arc::new(TokioRuntime)).expect("endpoint");
    endpoint.set_default_client_config(get_client_config(&Keypair::new()));
    let conn = endpoint.connect(addr, "localhost").expect("connect").await;
    (endpoint, conn)
}

fn closed(err: &ConnectionError) -> String {
    match err {
        ConnectionError::ApplicationClosed(close) => format!("closed {}", close.error_code),
        ConnectionError::TimedOut => "no handshake".into(),
        other => format!("error {other}"),
    }
}

#[tokio::main]
async fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let addr: SocketAddr = args.get(1).expect("ADDR").parse().expect("ADDR is HOST:PORT");
    match args[0].as_str() {
        "send" => {
            let (endpoint, conn) = connect(addr).await;
            let conn = conn.expect("handshake");
            for path in &args[2..] {
                let bytes = std::fs::read(path).expect("read");
                let mut stream = conn.open_uni().await.expect("stream");
                stream.write_all(&bytes).await.expect("write");
                stream.finish().expect("finish");
                let _ = stream.stopped().await;
                println!("sent {path}");
            }
            conn.close(0u32.into(), b"done");
            // The close leaves only when the endpoint sends it. A process that
            // exits first leaves the server holding the connection against the
            // per-peer cap until it times out.
            endpoint.wait_idle().await;
        }
        "oversized" => {
            let (_endpoint, conn) = connect(addr).await;
            let conn = conn.expect("handshake");
            let mut stream = conn.open_uni().await.expect("stream");
            let _ = stream.write_all(&vec![7u8; 4097]).await;
            let _ = stream.finish();
            println!("{}", closed(&conn.closed().await));
        }
        "connections" => {
            let n: usize = args.get(2).expect("N").parse().expect("N");
            // Back to back, because the per-IP rate is a token bucket that
            // refills. With gaps between them, a ninth connection in a minute
            // is allowed.
            let mut held = Vec::new();
            for _ in 0..n {
                held.push(tokio::time::timeout(Duration::from_secs(10), connect(addr)).await);
            }
            // A server over its per-peer cap accepts and then closes. The
            // last connection is the one a cap refuses. Its close comes after
            // the server kept every earlier one. So once it has closed, or
            // stayed quiet for 10 s, every line is settled.
            if let Some(Ok((_, Ok(conn)))) = held.last() {
                let _ = tokio::time::timeout(Duration::from_secs(10), conn.closed()).await;
            }
            for outcome in &held {
                match outcome {
                    Ok((_, Ok(conn))) => match conn.close_reason() {
                        Some(err) => println!("{}", closed(&err)),
                        None => println!("open"),
                    },
                    Ok((_, Err(err))) => println!("{}", closed(err)),
                    Err(_) => println!("no handshake"),
                }
            }
        }
        mode => panic!("unknown mode {mode}"),
    }
}
