package com.nexusriot.rezoagwe.core

import java.io.File
import java.security.MessageDigest

object Profiles {
    fun directory(root: File, cluster: String): File = File(root, "profiles/" +
        MessageDigest.getInstance("SHA-256").digest(cluster.toByteArray(Charsets.UTF_8)).joinToString("") { "%02x".format(it) })

    @Synchronized fun initialize(root: File, cluster: String): File {
        root.mkdirs()
        val owner = File(root, "legacy-profile.txt")
        if (!owner.exists()) owner.writeText(cluster)
        val dir = directory(root, cluster)
        check(dir.mkdirs() || dir.isDirectory) { "Cannot create cluster profile" }
        val migrated = File(dir, "legacy-migrated")
        if (owner.readText() == cluster && !migrated.exists()) {
            for (name in listOf("node.json", "node.json.wal", "bootstrap.json")) {
                val old = File(root, name); val target = File(dir, name)
                if (old.exists() && !target.exists()) old.copyTo(target, overwrite = false)
            }
        }
        migrated.writeText("done")
        return dir
    }
}
