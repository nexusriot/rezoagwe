package com.nexusriot.rezoagwe.net

import java.io.DataInputStream
import java.io.DataOutputStream
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.net.DatagramPacket
import java.net.DatagramSocket
import java.net.InetSocketAddress
import java.net.ServerSocket
import java.net.Socket
import java.net.SocketException
import kotlin.concurrent.thread

/** One received datagram. [from] is the source the socket saw, which need not be what the sender advertises. */
data class Packet(val from: String, val data: ByteArray) {
    override fun equals(other: Any?): Boolean =
        other is Packet && from == other.from && data.contentEquals(other.data)

    override fun hashCode(): Int = 31 * from.hashCode() + data.contentHashCode()
}

/** Largest datagram we are prepared to read; anything bigger cannot travel over UDP anyway. */
private const val MAX_DATAGRAM = 65535

/** Caps a stream frame so a corrupt length prefix cannot make the app allocate wildly. */
const val MAX_FRAME_SIZE = 64 shl 20

private const val DIAL_TIMEOUT_MS = 5_000
private const val STREAM_TIMEOUT_MS = 10_000

/**
 * Datagrams and streams on one port.
 *
 * Datagrams carry the protocol; streams carry payloads that do not fit one — a
 * full state sync or a large bootstrap roster. Both are framed identically, the
 * stream form simply adding a length prefix.
 */
class UdpTransport(
    bindPort: Int,
    private val onPacket: (Packet) -> Unit,
    private val onStream: (Socket) -> Unit,
    private val onError: (String, Throwable) -> Unit = { _, _ -> },
) {
    private val socket = DatagramSocket(null).apply {
        reuseAddress = true
        bind(InetSocketAddress(bindPort))
    }
    private val server: ServerSocket? = try {
        ServerSocket().apply {
            reuseAddress = true
            bind(InetSocketAddress(bindPort))
        }
    } catch (e: IOException) {
        // Not fatal: without a stream listener the node still works, falling back
        // to datagram state sync.
        onError("stream listener unavailable", e)
        null
    }

    @Volatile
    private var running = true

    val port: Int get() = socket.localPort

    /** False when the TCP port was taken, which drops state sync back to datagrams. */
    val streamsAvailable: Boolean get() = server != null

    init {
        thread(name = "rezoagwe-udp", isDaemon = true) { readLoop() }
        server?.let { thread(name = "rezoagwe-tcp", isDaemon = true) { acceptLoop(it) } }
    }

    fun send(addr: String, data: ByteArray) {
        val target = parseAddr(addr) ?: return
        socket.send(DatagramPacket(data, data.size, target))
    }

    fun dial(addr: String): Socket? {
        val target = parseAddr(addr) ?: return null
        return try {
            Socket().apply {
                connect(target, DIAL_TIMEOUT_MS)
                soTimeout = STREAM_TIMEOUT_MS
            }
        } catch (e: IOException) {
            null
        }
    }

    fun close() {
        running = false
        socket.close()
        try {
            server?.close()
        } catch (_: IOException) {
        }
    }

    private fun readLoop() {
        val buf = ByteArray(MAX_DATAGRAM)
        while (running) {
            val packet = DatagramPacket(buf, buf.size)
            try {
                socket.receive(packet)
            } catch (e: SocketException) {
                return // socket closed
            } catch (e: IOException) {
                if (running) onError("read datagram", e)
                continue
            }
            val data = packet.data.copyOfRange(packet.offset, packet.offset + packet.length)
            val from = "${packet.address.hostAddress}:${packet.port}"
            try {
                onPacket(Packet(from, data))
            } catch (e: Exception) {
                onError("handle datagram", e)
            }
        }
    }

    private fun acceptLoop(server: ServerSocket) {
        while (running) {
            val conn = try {
                server.accept()
            } catch (e: IOException) {
                return
            }
            conn.soTimeout = STREAM_TIMEOUT_MS
            thread(isDaemon = true) {
                try {
                    onStream(conn)
                } catch (e: Exception) {
                    onError("handle stream", e)
                } finally {
                    try {
                        conn.close()
                    } catch (_: IOException) {
                    }
                }
            }
        }
    }

    companion object {
        /** Parses "host:port", treating a missing host as the loopback interface. */
        fun parseAddr(addr: String): InetSocketAddress? {
            val i = addr.lastIndexOf(':')
            if (i < 0) return null
            val host = addr.substring(0, i).ifEmpty { "127.0.0.1" }.trim('[', ']')
            val port = addr.substring(i + 1).toIntOrNull() ?: return null
            if (port !in 1..65535) return null
            return try {
                InetSocketAddress(host, port)
            } catch (e: IllegalArgumentException) {
                null
            }
        }
    }
}

/** Writes a length-prefixed frame. */
fun writeFrame(out: OutputStream, data: ByteArray) {
    require(data.size <= MAX_FRAME_SIZE) { "frame exceeds maximum size" }
    DataOutputStream(out).apply {
        writeInt(data.size)
        write(data)
        flush()
    }
}

/** Reads a length-prefixed frame. */
fun readFrame(input: InputStream): ByteArray {
    val stream = DataInputStream(input)
    val size = stream.readInt()
    if (size < 0 || size > MAX_FRAME_SIZE) throw IOException("frame exceeds maximum size")
    val buf = ByteArray(size)
    stream.readFully(buf)
    return buf
}
