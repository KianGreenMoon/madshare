package ygg.daemonlord.madshare;

import android.Manifest;
import android.content.pm.PackageManager;
import android.os.Build;
import android.os.Bundle;
import android.webkit.WebView;

import androidx.core.app.ActivityCompat;
import androidx.core.content.ContextCompat;

import com.getcapacitor.BridgeActivity;

/**
 * Capacitor host activity. Beyond the default BridgeActivity it installs the native
 * media bridge ({@link MediaBridge}) on the WebView so the player — which runs on the
 * REMOTE server origin, where Capacitor does NOT inject its plugin bridge — can drive
 * OS media controls and a background-playback foreground service (design doc §6).
 */
public class MainActivity extends BridgeActivity {
    private static final int REQ_POST_NOTIFICATIONS = 7001;

    @Override
    public void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);

        // addJavascriptInterface is injected into EVERY page the WebView navigates to
        // (any origin), so window.MadshareMedia is present on the remote server page.
        // It takes effect on the next page load; the launcher is already loaded but
        // does not use it — the remote navigation that follows does.
        WebView webView = getBridge().getWebView();
        webView.addJavascriptInterface(new MediaBridge(this), "MadshareMedia");
        MediaBridge.attachWebView(webView);

        // Android 13+: the media notification (and thus the visible controls) needs
        // runtime notification permission. The foreground service still runs without
        // it; only the notification is suppressed.
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU
                && ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED) {
            ActivityCompat.requestPermissions(this,
                    new String[]{ Manifest.permission.POST_NOTIFICATIONS }, REQ_POST_NOTIFICATIONS);
        }
    }

    @Override
    public void onDestroy() {
        MediaBridge.detach();
        super.onDestroy();
    }
}
