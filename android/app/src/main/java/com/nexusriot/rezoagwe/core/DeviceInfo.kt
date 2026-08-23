package com.nexusriot.rezoagwe.core

import android.content.Context
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.os.Build
import android.os.PowerManager
import java.net.Inet4Address
import java.net.Inet6Address
import java.net.NetworkInterface

data class InterfaceInfo(
    val name: String,
    val up: Boolean,
    val loopback: Boolean,
    val ipv4: List<String> = emptyList(),
    val ipv6: List<String> = emptyList(),
)

/**
 * The parts of the phone that decide whether a gossip node can do its job.
 *
 * A node that cannot be reached is indistinguishable from a node with no peers,
 * and on Android the usual causes are not in the protocol at all: no network, a
 * metered link, or Doze stopping the timers.
 */
data class DeviceReport(
    val model: String = "",
    val androidRelease: String = "",
    val sdk: Int = 0,
    val transport: String = "unknown",
    val metered: Boolean = false,
    val vpn: Boolean = false,
    val validated: Boolean = false,
    val interfaces: List<InterfaceInfo> = emptyList(),
    val ignoringBatteryOptimizations: Boolean = false,
    val powerSaveMode: Boolean = false,
)

fun deviceReport(context: Context): DeviceReport {
    val connectivity = context.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager
    val capabilities = connectivity?.activeNetwork?.let { connectivity.getNetworkCapabilities(it) }
    val power = context.getSystemService(Context.POWER_SERVICE) as? PowerManager
    return DeviceReport(
        model = "${Build.MANUFACTURER} ${Build.MODEL}",
        androidRelease = Build.VERSION.RELEASE ?: "",
        sdk = Build.VERSION.SDK_INT,
        transport = transportName(capabilities),
        metered = connectivity?.isActiveNetworkMetered ?: false,
        vpn = capabilities?.hasTransport(NetworkCapabilities.TRANSPORT_VPN) ?: false,
        validated = capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED) ?: false,
        interfaces = networkInterfaces(),
        ignoringBatteryOptimizations = power?.isIgnoringBatteryOptimizations(context.packageName) ?: false,
        powerSaveMode = power?.isPowerSaveMode ?: false,
    )
}

private fun transportName(capabilities: NetworkCapabilities?): String = when {
    capabilities == null -> "none"
    capabilities.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> "Wi-Fi"
    capabilities.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "cellular"
    capabilities.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> "ethernet"
    capabilities.hasTransport(NetworkCapabilities.TRANSPORT_VPN) -> "VPN"
    capabilities.hasTransport(NetworkCapabilities.TRANSPORT_BLUETOOTH) -> "Bluetooth"
    else -> "other"
}

/**
 * Every interface with an address, loopback included.
 *
 * The address a node advertises has to come from here; showing the whole list is
 * what makes "peers cannot reach me" diagnosable on a phone with a VPN, a hotspot
 * and Wi-Fi all up at once.
 */
fun networkInterfaces(): List<InterfaceInfo> = try {
    NetworkInterface.getNetworkInterfaces().toList().map { nic ->
        val addresses = nic.inetAddresses.toList()
        InterfaceInfo(
            name = nic.name,
            up = nic.isUp,
            loopback = nic.isLoopback,
            ipv4 = addresses.filterIsInstance<Inet4Address>().mapNotNull { it.hostAddress },
            ipv6 = addresses.filterIsInstance<Inet6Address>().mapNotNull { it.hostAddress },
        )
    }.filter { it.ipv4.isNotEmpty() || it.ipv6.isNotEmpty() }
        .sortedWith(compareBy({ it.loopback }, { it.name }))
} catch (e: Exception) {
    emptyList()
}
