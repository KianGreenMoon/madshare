package ygg.daemonlord.madshare;

import android.Manifest;
import android.content.pm.PackageManager;
import android.os.Build;
import android.os.Bundle;
import android.webkit.WebView;

import androidx.activity.OnBackPressedCallback;
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
        // The bundled launcher's URL (e.g. https://localhost); openLauncher() reloads it
        // to return from a remote server to the server picker, and getLocalUrl() is the
        // origin the back handler uses to tell "are we on the launcher?".
        String launcherUrl = getBridge().getAppUrl();
        webView.addJavascriptInterface(new MediaBridge(this, launcherUrl), "MadshareMedia");
        MediaBridge.attachWebView(webView);

        installBackHandler(webView, getBridge().getLocalUrl());

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

    /**
     * Hardware back-button policy (design doc §10 Q1). Capacitor ships no back
     * handling, so by default Android finishes the activity (closing the app) on
     * every back press. Instead:
     *   • on a remote server page with WebView history → go back one page;
     *   • at the server's library — the back-stack root, because the launcher hands
     *     off with location.replace() and is never left on the stack → stay put: no
     *     exit, and never re-open the launcher (that is the explicit "Switch server"
     *     control's job);
     *   • on the launcher screen itself (the genuine app root) → exit the app.
     * Closing the app from inside a server is left to Android's task switcher.
     */
    private void installBackHandler(WebView webView, String localOrigin) {
        // Match the launcher on an origin boundary so a server that merely shares the
        // "localhost" prefix (e.g. https://localhost:3000) is not mistaken for it.
        String launcherExact = localOrigin == null ? null : localOrigin;
        String launcherPrefix = localOrigin == null ? null : localOrigin + "/";

        getOnBackPressedDispatcher().addCallback(this, new OnBackPressedCallback(true) {
            @Override
            public void handleOnBackPressed() {
                String current = webView.getUrl();
                boolean onLauncher = current != null && launcherExact != null
                        && (current.equals(launcherExact) || current.startsWith(launcherPrefix));
                if (onLauncher) {
                    finish();              // genuine app root → exit the app
                } else if (webView.canGoBack()) {
                    webView.goBack();      // in-page history on the server origin
                }
                // else: at the server's library root → stay put (no exit, no launcher).
            }
        });
    }

    @Override
    public void onDestroy() {
        MediaBridge.detach();
        super.onDestroy();
    }
}
