package com.nexusriot.rezoagwe
import com.nexusriot.rezoagwe.core.*
import java.io.File
import kotlinx.serialization.json.*
import org.junit.Assert.*
import org.junit.Test
import org.junit.Rule
import org.junit.rules.TemporaryFolder
class JournalTest {
 @get:Rule val tmp=TemporaryFolder()
 @Test fun replayCheckpointAndTruncatedTail() {
  for(mode in listOf("journal","journal-sync")) {
   val file=File(tmp.root,"$mode.json");val p=Persister(file,mode)
   for(i in 1..65) {p.save(i.toLong(),PersistState(clock=i.toLong(),entries=mapOf("k" to KvEntry(value="$i"))));assertEquals("",p.health.error)}
   assertEquals(mode=="journal-sync",p.health.durable)
   File(file.path+".wal").appendText("{torn")
   val q=Persister(file,mode);assertEquals("65",q.load()!!.entries["k"]!!.value)
   q.save(66,PersistState(clock=66));assertTrue(Persister(file).load()!!.entries.isEmpty())
  }
 }
 @Test fun corruptionBlocksSaving() {
  val file=File(tmp.root,"state.json");val p=Persister(file,"journal");p.save(1,PersistState(clock=1));val wal=File(file.path+".wal");wal.appendText("{bad}\n");val before=wal.readText();val q=Persister(file);assertNull(q.load());q.save(2,PersistState());assertEquals(before,wal.readText());assertTrue(q.health.error.isNotEmpty())
 }
 @Test fun migrationRetainsJournal() {
  val file=File(tmp.root,"node.json");val p=Persister(file,"journal");p.save(1,PersistState(clock=1,entries=mapOf("k" to KvEntry(value="journal-only"))))
  val dir=Profiles.initialize(tmp.root,"test");assertEquals("journal-only",Persister(File(dir,"node.json")).load()!!.entries["k"]!!.value)
 }
 @Test fun missingCheckpointBlocksPartialRecovery() {
  val file=File(tmp.root,"node.json");val p=Persister(file,"journal")
  for(i in 1..65) p.save(i.toLong(),PersistState(clock=i.toLong(),entries=mapOf("stable" to KvEntry(value="keep"),"changed" to KvEntry(value="$i"))))
  val wal=File(file.path+".wal");val before=wal.readText();assertTrue(file.delete());val q=Persister(file)
  assertNull(q.load());assertTrue(q.loadError.contains("checkpoint"));q.save(1,PersistState());assertEquals(before,wal.readText());assertFalse(file.exists())
 }
 @Test fun sharedGoAndElectronJournal() {
  val fixture=listOf(File("../testdata/journal.json"),File("../../testdata/journal.json")).first { it.exists() }
  val corpus=Json.parseToJsonElement(fixture.readText()).jsonObject
  val file=File(tmp.root,"state.json");file.writeText(corpus.getValue("checkpoint").toString());File(file.path+".wal").writeText(corpus.getValue("records").jsonArray.joinToString("\n",postfix="\n"))
  val p=Persister(file);val state=p.load();assertEquals("",p.loadError);assertNotNull(state)
  assertEquals(5L,state!!.clock);assertEquals("journal-fixture",state.nodeId);assertEquals("checkpoint value",state.entries["keep"]!!.value);assertEquals("Go <>& 😀\nrecord",state.entries["go"]!!.value);assertEquals("JS 😀 <>&",state.entries["__proto__"]!!.value);assertTrue(state.entries["deleted"]!!.deleted);assertFalse(state.entries.containsKey("removed"))
 }
}
