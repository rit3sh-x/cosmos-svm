//! svm-tpu is Agave's QUIC transaction port for svmd. It runs Agave's own
//! `solana-streamer` 4.3.0 server with an empty stake map, so every sender is
//! unstaked, as Agave treats a sender with a random key.
//!
//! svmd starts it as a child and reads frames from its standard output:
//!
//! ```text
//! frame   type u8 || length u16 LE || payload
//! type 0  transaction: a stream's bytes, at most 4,096
//! type 1  ready: the UDP socket is bound; the payload is its address
//! type 2  stats: the streamer's counters, as JSON, each time they report
//! ```
//!
//! The streamer's counters are private to its crate, and its only outlet is
//! `solana-metrics`. So this process points `solana-metrics` at a receiver of
//! its own on loopback, and turns each report into a stats frame. Nothing
//! leaves the machine.
//!
//! When a write to standard output fails, the process exits. That is how it
//! ends with its parent.

use {
    crossbeam_channel::{Sender, bounded},
    solana_keypair::Keypair,
    solana_streamer::{
        nonblocking::swqos::SwQosConfig,
        packet::PacketBatch,
        quic::{QuicStreamerConfig, SpawnServerResult, spawn_stake_weighted_qos_server},
        quic_socket::QuicSocket,
        streamer::StakedNodes,
    },
    std::{
        collections::HashMap,
        io::{BufRead, BufReader, Read, Write},
        net::{SocketAddr, TcpListener, UdpSocket},
        num::NonZeroUsize,
        process::exit,
        sync::{Arc, Mutex, RwLock},
    },
    tokio_util::sync::CancellationToken,
};

/// The stream limit Agave's validator sets.
const MAX_TRANSACTION: u32 = 4096;

/// The queue between the streamer and the writer, in packets. The streamer
/// sends each packet as a batch of one. When the queue is full, the streamer
/// drops the new packet with `try_send` and counts it as
/// `total_handle_chunk_to_packet_send_full_err`, as on Agave's TPU.
///
/// Agave's channel holds 50,000 because sigverify drains it in batches. Here
/// svmd admits one frame at a time, so a deeper queue holds only memory and
/// old transactions. A packet keeps alive the receive buffer it was read
/// from. That buffer is at most 64 KiB with GRO, so the queue holds at most
/// 64 MiB.
const QUEUE: usize = 1_024;

/// The name the streamer's counters report under.
const METRICS_NAME: &str = "svm_tpu_quic";

const FRAME_TRANSACTION: u8 = 0;
const FRAME_READY: u8 = 1;
const FRAME_STATS: u8 = 2;

struct Args {
    bind: String,
    per_peer: usize,
    per_ip_per_minute: u64,
    unstaked: usize,
}

/// The three bounds Agave's validator exposes for an unstaked sender, with its
/// names and defaults.
fn args(mut it: impl Iterator<Item = String>) -> Args {
    let defaults = SwQosConfig::default();
    let mut a = Args {
        bind: String::new(),
        per_peer: defaults.max_connections_per_unstaked_peer,
        per_ip_per_minute: QuicStreamerConfig::default().max_connections_per_ipaddr_per_min,
        unstaked: defaults.max_unstaked_connections,
    };
    while let Some(flag) = it.next() {
        let value = it.next().unwrap_or_else(|| fail(&format!("{flag} wants a value")));
        let number = || value.parse::<u64>().unwrap_or_else(|_| fail(&format!("{flag}: {value} is not a count")));
        match flag.as_str() {
            "--bind" => a.bind = value.clone(),
            "--max-connections-per-unstaked-peer" => a.per_peer = number() as usize,
            "--max-connections-per-ipaddr-per-minute" => a.per_ip_per_minute = number(),
            "--max-unstaked-connections" => a.unstaked = number() as usize,
            _ => fail(&format!("unknown flag {flag}")),
        }
    }
    if a.bind.is_empty() {
        fail("--bind HOST:PORT is required");
    }
    a
}

fn fail(why: &str) -> ! {
    eprintln!("svm-tpu: {why}");
    exit(2)
}

/// Frames is standard output, shared by the transaction writer and the stats
/// receiver. A write that fails means svmd has gone, and this process ends.
#[derive(Clone)]
struct Frames(Arc<Mutex<std::io::Stdout>>);

impl Frames {
    fn write(&self, kind: u8, payload: &[u8]) {
        let mut out = self.0.lock().unwrap_or_else(|e| e.into_inner());
        let mut frame = Vec::with_capacity(3 + payload.len());
        frame.push(kind);
        frame.extend_from_slice(&(payload.len() as u16).to_le_bytes());
        frame.extend_from_slice(payload);
        if out.write_all(&frame).and_then(|()| out.flush()).is_err() {
            exit(0);
        }
    }
}

/// Logger enables Info so `datapoint_info!` submits, and prints Warn and
/// above to standard error, which svmd puts in its own log.
struct Logger;

impl log::Log for Logger {
    fn enabled(&self, m: &log::Metadata) -> bool {
        m.level() <= log::Level::Info
    }
    fn log(&self, r: &log::Record) {
        if r.level() <= log::Level::Warn {
            eprintln!("svm-tpu: {} {}: {}", r.level(), r.target(), r.args());
        }
    }
    fn flush(&self) {}
}

static LOGGER: Logger = Logger;

/// report_to points `solana-metrics` at a receiver of this process's own on
/// loopback, and hands `emit` each report of the streamer's counters as JSON.
fn report_to(emit: impl Fn(&str) + Send + 'static) {
    let metrics = TcpListener::bind("127.0.0.1:0").unwrap_or_else(|e| fail(&format!("metrics receiver: {e}")));
    let receiver_addr = metrics.local_addr().unwrap_or_else(|e| fail(&e.to_string()));
    // SAFETY: it runs before any thread of this process that reads the
    // environment, and solana-metrics reads this once, when its agent first
    // starts.
    unsafe {
        std::env::set_var(
            "SOLANA_METRICS_CONFIG",
            format!("host=http://{receiver_addr},db=svm,u=svm,p=svm"),
        );
    }
    let _ = log::set_logger(&LOGGER);
    log::set_max_level(log::LevelFilter::Info);
    std::thread::spawn(move || receive_metrics(metrics, emit));
}

/// receive_metrics serves the InfluxDB write endpoint solana-metrics posts to,
/// and hands each point of the streamer's report to `emit`. Each request is
/// answered and its connection closed.
fn receive_metrics(listener: TcpListener, emit: impl Fn(&str)) {
    for conn in listener.incoming().flatten() {
        let mut reader = BufReader::new(conn);
        let mut length = 0usize;
        let mut line = String::new();
        loop {
            line.clear();
            if reader.read_line(&mut line).unwrap_or(0) == 0 {
                break;
            }
            let header = line.trim_end();
            if header.is_empty() {
                break;
            }
            if let Some((name, value)) = header.split_once(':')
                && name.eq_ignore_ascii_case("content-length")
            {
                length = value.trim().parse().unwrap_or(0);
            }
        }
        let mut body = vec![0u8; length.min(1 << 20)];
        if reader.read_exact(&mut body).is_err() {
            continue;
        }
        for point in String::from_utf8_lossy(&body).lines() {
            if let Some(json) = stats_json(point) {
                emit(&json);
            }
        }
        let _ = reader
            .get_mut()
            .write_all(b"HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n");
    }
}

/// stats_json is one InfluxDB line of the streamer's report as a JSON object
/// of its integer fields, or None for any other point.
fn stats_json(point: &str) -> Option<String> {
    let mut parts = point.split(' ');
    let measurement = parts.next()?.split(',').next()?;
    if measurement != METRICS_NAME {
        return None;
    }
    let fields = parts
        .next()?
        .split(',')
        .filter_map(|f| {
            let (name, value) = f.split_once('=')?;
            let value: i64 = value.strip_suffix('i')?.parse().ok()?;
            Some(format!("\"{name}\":{value}"))
        })
        .collect::<Vec<_>>();
    Some(format!("{{{}}}", fields.join(",")))
}

/// serve binds the port and starts the streamer on it, which sends each packet
/// to `sender`. It returns the bound address and the server, to keep alive.
fn serve(a: &Args, sender: Sender<PacketBatch>) -> (SocketAddr, SpawnServerResult) {
    let socket = UdpSocket::bind(&a.bind).unwrap_or_else(|e| fail(&format!("bind {}: {e}", a.bind)));
    socket
        .set_nonblocking(true)
        .unwrap_or_else(|e| fail(&format!("nonblocking: {e}")));
    let bound = socket.local_addr().unwrap_or_else(|e| fail(&e.to_string()));
    let staked = Arc::new(RwLock::new(StakedNodes::new(Arc::new(HashMap::new()), HashMap::new())));
    // The receive thread count is the validator's default, the CPU count
    // capped at eight.
    let threads = std::thread::available_parallelism().map_or(1, |n| n.get()).min(8);
    let server = spawn_stake_weighted_qos_server(
        "svmTpuQuic",
        METRICS_NAME,
        [QuicSocket::from(socket)],
        &Keypair::new(),
        sender,
        staked,
        QuicStreamerConfig {
            max_connections_per_ipaddr_per_min: a.per_ip_per_minute,
            num_threads: NonZeroUsize::new(threads).unwrap_or(NonZeroUsize::MIN),
            stream_receive_window_size: MAX_TRANSACTION,
            max_stream_data_bytes: MAX_TRANSACTION,
            ..QuicStreamerConfig::default()
        },
        SwQosConfig {
            max_unstaked_connections: a.unstaked,
            max_connections_per_unstaked_peer: a.per_peer,
            ..SwQosConfig::default()
        },
        CancellationToken::new(),
    )
    .unwrap_or_else(|e| fail(&format!("streamer: {e:?}")));
    (bound, server)
}

fn main() {
    let a = args(std::env::args().skip(1));
    let frames = Frames(Arc::new(Mutex::new(std::io::stdout())));
    {
        let frames = frames.clone();
        report_to(move |json| frames.write(FRAME_STATS, json.as_bytes()));
    }

    let (sender, receiver) = bounded(QUEUE);
    let (bound, _server) = serve(&a, sender);
    frames.write(FRAME_READY, bound.to_string().as_bytes());

    for batch in receiver {
        for packet in batch.iter() {
            let packet = packet.to_bytes_packet();
            frames.write(FRAME_TRANSACTION, packet.buffer());
        }
    }
}

#[cfg(test)]
mod tests {
    use {
        super::*,
        quinn::{Connection, Endpoint, EndpointConfig, TokioRuntime},
        solana_streamer::nonblocking::testing_utilities::get_client_config,
        std::{sync::mpsc, time::Duration},
        tokio::runtime::Runtime,
    };

    /// The streamer's count of packets dropped because the queue was full.
    const DROPPED: &str = "\"total_handle_chunk_to_packet_send_full_err\":";

    /// The deadline that stops a test that would otherwise hang. It decides
    /// nothing: the streamer reports every 5 s and solana-metrics posts every
    /// 10 s.
    const HANG: Duration = Duration::from_secs(120);

    /// index is the number a test packet carries.
    fn index(batch: PacketBatch) -> u32 {
        let packet = batch.iter().next().expect("a packet");
        u32::from_le_bytes(packet.data(..).expect("bytes").try_into().expect("four bytes"))
    }

    /// One connection, held with its endpoint, as tools/quic-client opens it.
    fn connect(rt: &Runtime, addr: SocketAddr) -> (Endpoint, Connection) {
        rt.block_on(async {
            let socket = UdpSocket::bind("127.0.0.1:0").expect("bind");
            let mut endpoint =
                Endpoint::new(EndpointConfig::default(), None, socket, Arc::new(TokioRuntime)).expect("endpoint");
            endpoint.set_default_client_config(get_client_config(&Keypair::new()));
            let conn = endpoint.connect(addr, "localhost").expect("connect").await.expect("handshake");
            (endpoint, conn)
        })
    }

    /// With nothing draining the queue, a flood past it leaves the first
    /// QUEUE packets queued, and the streamer drops each later one and
    /// reports the drop. One connection's streams are handled in order, so
    /// once the queue is drained, the next packet is the next one sent.
    #[test]
    fn a_full_queue_drops_new_packets_and_reports_them() {
        const EXTRA: u32 = 64;
        let (drops, reports) = mpsc::channel();
        report_to(move |json| {
            if let Some(rest) = json.split(DROPPED).nth(1) {
                let n: u64 = rest.split([',', '}']).next().and_then(|n| n.parse().ok()).expect("a count");
                let _ = drops.send(n);
            }
        });
        let (sender, receiver) = bounded(QUEUE);
        let (addr, _server) = serve(&args(["--bind", "127.0.0.1:0"].map(String::from).into_iter()), sender);

        let rt = Runtime::new().expect("runtime");
        let (_endpoint, conn) = connect(&rt, addr);
        let send = |i: u32| {
            rt.block_on(async {
                let mut stream = conn.open_uni().await.expect("stream");
                stream.write_all(&i.to_le_bytes()).await.expect("write");
                stream.finish().expect("finish");
            })
        };
        for i in 0..QUEUE as u32 + EXTRA {
            send(i);
        }

        let mut dropped = 0;
        while dropped < u64::from(EXTRA) {
            dropped += reports.recv_timeout(HANG).expect("a report of the drops");
        }
        assert_eq!(dropped, u64::from(EXTRA));
        assert_eq!(receiver.len(), QUEUE);
        for i in 0..QUEUE as u32 {
            assert_eq!(index(receiver.recv().expect("a queued packet")), i);
        }
        send(u32::MAX);
        assert_eq!(index(receiver.recv_timeout(HANG).expect("the packet after the drain")), u32::MAX);
    }
}
