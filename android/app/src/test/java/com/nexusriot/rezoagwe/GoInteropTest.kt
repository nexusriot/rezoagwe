package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.NodeConfig
import com.nexusriot.rezoagwe.core.NodeEngine
import java.net.HttpURLConnection
import java.net.ServerSocket
import java.net.URL
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assume.assumeTrue
import org.junit.Test

/**
 * Live interop against a running Go cluster.
 *
 * The byte-level parity vectors in [CodecTest] prove the framing agrees; this
 * proves the whole conversation does — bootstrap, gossip, replication — against
 * the real Go implementation rather than a fixture of it.
 *
 * Skipped unless a cluster is pointed at:
 *
 *     rezoagwe-bootstrap -headless -port 19999 -psk demo -cluster e2e -data -
 *     rezoagwe-discovery -headless -node 127.0.0.1:13137 -bootstrap 127.0.0.1:19999 \
 *         -psk demo -cluster e2e -http 127.0.0.1:18081 -data -
 *     REZOAGWE_GO_BOOTSTRAP=127.0.0.1:19999 REZOAGWE_GO_HTTP=127.0.0.1:18081 \
 *         ./gradlew :app:testDebugUnitTest --tests '*GoInteropTest'
 */
class GoInteropTest {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private var engine: NodeEngine? = null

    private val bootstrap: String? = System.getenv("REZOAGWE_GO_BOOTSTRAP")
    private val goHttp: String? = System.getenv("REZOAGWE_GO_HTTP")
    private val psk: String = System.getenv("REZOAGWE_GO_PSK") ?: "demo"
    private val cluster: String = System.getenv("REZOAGWE_GO_CLUSTER") ?: "e2e"

    @After
    fun tearDown() {
        engine?.let { runCatching { it.stop() } }
        scope.cancel()
    }

    @Test
    fun replicatesWithAGoCluster() {
        assumeTrue("no Go cluster configured", bootstrap != null && goHttp != null)

        val port = ServerSocket(0).use { it.localPort }
        val node = NodeEngine(
            NodeConfig(
                advertiseHost = "127.0.0.1",
                port = port,
                nick = "android",
                seeds = listOf(bootstrap!!),
                psk = psk,
                cluster = cluster,
                gossipIntervalMs = 1_000,
                heartbeatIntervalMs = 1_000,
            ),
            dataFile = null,
            scope = scope,
        )
        engine = node
        node.start()

        waitFor("the Go cluster to appear in the peer list") { node.peerList.value.isNotEmpty() }

        // Android → Go.
        val key = "from-android-$port"
        node.set(key, "kotlin-wrote-this")
        waitFor("the Go node to receive the write") { httpGet("/kv/$key") == "kotlin-wrote-this" }

        // Go → Android.
        httpPut("/kv/from-go-$port", "go-wrote-this")
        waitFor("the write to reach the Kotlin store") { node.store["from-go-$port"] == "go-wrote-this" }

        // A delete has to travel as a versioned tombstone in both directions.
        node.delete(key)
        waitFor("the Go node to apply the delete") { httpStatus("/kv/$key") == 404 }

        assertEquals(0, node.metrics.authFailures.get())
    }

    private fun waitFor(what: String, timeoutMs: Long = 20_000, condition: () -> Boolean) {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (System.currentTimeMillis() < deadline) {
            if (runCatching(condition).getOrDefault(false)) return
            Thread.sleep(50)
        }
        throw AssertionError("timed out waiting for $what")
    }

    private fun httpGet(path: String): String? {
        val conn = URL("http://$goHttp$path").openConnection() as HttpURLConnection
        return try {
            if (conn.responseCode != 200) null else conn.inputStream.bufferedReader().readText()
        } finally {
            conn.disconnect()
        }
    }

    private fun httpStatus(path: String): Int {
        val conn = URL("http://$goHttp$path").openConnection() as HttpURLConnection
        return try {
            conn.responseCode
        } finally {
            conn.disconnect()
        }
    }

    private fun httpPut(path: String, body: String) {
        val conn = (URL("http://$goHttp$path").openConnection() as HttpURLConnection).apply {
            requestMethod = "PUT"
            doOutput = true
        }
        try {
            conn.outputStream.use { it.write(body.toByteArray()) }
            check(conn.responseCode in 200..299) { "PUT $path failed: ${conn.responseCode}" }
        } finally {
            conn.disconnect()
        }
    }
}
