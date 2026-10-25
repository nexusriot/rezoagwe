package com.nexusriot.rezoagwe

import com.nexusriot.rezoagwe.core.KvStore
import com.nexusriot.rezoagwe.proto.*
import java.io.File
import kotlinx.serialization.json.*
import org.junit.Assert.*
import org.junit.Test

class BoundaryCorpusTest {
    @Test fun sharedCorpus() {
        val file = listOf(File("../testdata/boundaries.json"), File("../../testdata/boundaries.json")).first { it.exists() }
        val corpus = Json.parseToJsonElement(file.readText()).jsonObject
        val store = KvStore("test")
        for (raw in corpus.getValue("entries").jsonArray) {
            val e = raw.jsonObject
            assertEquals(e.getValue("escaped_bytes").jsonObject.getValue("kotlin").jsonPrimitive.int, escapedLen(e.getValue("value").jsonPrimitive.content))
            assertTrue(store.apply(KVUpdate(action = KVAction.SET, key = e.getValue("key").jsonPrimitive.content,
                value = e.getValue("value").jsonPrimitive.content,
                version = Version(e.getValue("counter").jsonPrimitive.long, e.getValue("node").jsonPrimitive.content),
                expiresAt = e.getValue("expires_at").jsonPrimitive.long)))
        }
        assertEquals(corpus.getValue("sorted_keys").jsonArray.map { it.jsonPrimitive.content },store.updates().map { it.key })
        assertEquals(corpus.getValue("buckets").jsonArray.map { it.jsonPrimitive.content },store.fingerprint(digestVersion = 2).buckets)
    }
}
