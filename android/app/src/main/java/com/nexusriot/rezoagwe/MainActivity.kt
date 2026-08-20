package com.nexusriot.rezoagwe

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.core.content.ContextCompat
import com.nexusriot.rezoagwe.core.Runtime
import com.nexusriot.rezoagwe.ui.RezoagweApp
import com.nexusriot.rezoagwe.ui.theme.RezoagweTheme

class MainActivity : ComponentActivity() {
    private val requestNotifications =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        Runtime.init(this)
        askForNotifications()
        setContent {
            RezoagweTheme {
                RezoagweApp()
            }
        }
    }

    /**
     * The foreground service needs a visible notification to keep the node alive.
     * Without the permission the service still runs, but the user loses the only
     * indication that their phone is gossiping.
     */
    private fun askForNotifications() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return
        val granted = ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) ==
            PackageManager.PERMISSION_GRANTED
        if (!granted) requestNotifications.launch(Manifest.permission.POST_NOTIFICATIONS)
    }
}
