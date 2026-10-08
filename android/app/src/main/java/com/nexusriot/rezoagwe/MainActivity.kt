package com.nexusriot.rezoagwe

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.core.content.ContextCompat
import com.nexusriot.rezoagwe.core.Runtime
import com.nexusriot.rezoagwe.service.NodeService
import com.nexusriot.rezoagwe.ui.RezoagweApp
import com.nexusriot.rezoagwe.ui.theme.RezoagweTheme

class MainActivity : ComponentActivity() {
    private val requestNotifications =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Android 15 draws apps edge to edge whether they ask or not; opting in
        // here means every API level gets the same insets, which the UI pads for.
        enableEdgeToEdge()
        Runtime.init(this)
        resumeWantedRoles()
        askForNotifications()
        setContent {
            RezoagweTheme {
                RezoagweApp()
            }
        }
    }

    /**
     * Brings back the roles the user left running.
     *
     * The intent is persisted, but the only thing that ever read it was
     * NodeService.onStartCommand — which cannot run after a force-stop or a
     * reboot, because those are exactly the cases where no service is left to
     * restart. Opening the app then showed a stopped node with "Start node"
     * offered, while shared_prefs still recorded that the user wanted it up.
     *
     * Starting the service is enough: its onStartCommand restores every wanted
     * role off the main thread, and is idempotent when one is already running.
     */
    private fun resumeWantedRoles() {
        if (Runtime.anyRoleWanted(this)) NodeService.ensureRunning(this)
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
