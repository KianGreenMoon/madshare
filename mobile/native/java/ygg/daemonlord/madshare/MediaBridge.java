package ygg.daemonlord.madshare;

import android.content.Context;
import android.content.Intent;
import android.os.Handler;
import android.os.Looper;
import android.webkit.JavascriptInterface;
import android.webkit.WebView;

import androidx.core.content.ContextCompat;

import org.json.JSONObject;

import java.lang.ref.WeakReference;

/**
 * JS-facing media bridge, injected into EVERY page the WebView loads as
 * {@code window.MadshareMedia} via {@link WebView#addJavascriptInterface}. This is
 * the key difference from Capacitor's plugin bridge, which is NOT injected on the
 * remote server origin where the player actually runs (see design doc §6).
 *
 * <p>The web player ({@code webui/static/js/media-session.js}) calls the
 * {@code @JavascriptInterface} methods to push metadata / playback state / position;
 * the bridge forwards a snapshot to {@link MediaPlaybackService}, which owns the
 * {@link android.support.v4.media.session.MediaSessionCompat} and the foreground
 * notification. Transport events (notification buttons, lock screen, headset) travel
 * the other way: the service's session callback calls {@link #dispatch}/{@link #dispatchSeek},
 * which run {@code window.__madshareMediaAction(...)} back in the page.
 *
 * <p>NOTE: {@code @JavascriptInterface} methods run on a binder thread, so they only
 * fire Intents (thread-safe); anything touching the WebView is posted to the main thread.
 */
public class MediaBridge {
    private static WeakReference<WebView> webViewRef = new WeakReference<>(null);
    private static final Handler MAIN = new Handler(Looper.getMainLooper());

    private final Context appContext;

    // Latest snapshot, sent whole on every change (updates are infrequent: track
    // change + play/pause, not per-frame — the OS extrapolates the scrubber).
    private String title = "", artist = "", album = "";
    private double durationMs = 0, positionMs = 0, rate = 1.0;
    private boolean playing = false;
    private boolean active = false; // whether the foreground service is running

    MediaBridge(Context ctx) { this.appContext = ctx.getApplicationContext(); }

    /** Wire the WebView the service should call back into. Cleared in onDestroy. */
    static void attachWebView(WebView wv) { webViewRef = new WeakReference<>(wv); }
    static void detach() { webViewRef = new WeakReference<>(null); }

    @JavascriptInterface
    public void setMetadata(String json) {
        try {
            JSONObject o = new JSONObject(json);
            title = o.optString("title", "");
            artist = o.optString("artist", "");
            album = o.optString("album", "");
        } catch (Exception ignored) { /* malformed payload — keep previous metadata */ }
        push();
    }

    @JavascriptInterface
    public void setPlaybackState(String state) {
        playing = "playing".equals(state);
        push();
    }

    @JavascriptInterface
    public void setPositionState(double durMs, double posMs, double playbackRate) {
        durationMs = durMs;
        positionMs = posMs;
        rate = playbackRate <= 0 ? 1.0 : playbackRate;
        push();
    }

    /** Tear down the session + notification (queue emptied / player stopped). */
    @JavascriptInterface
    public void clear() {
        if (!active) return;
        active = false;
        appContext.startService(new Intent(appContext, MediaPlaybackService.class)
                .setAction(MediaPlaybackService.ACTION_STOP));
    }

    private void push() {
        // Android 14+ forbids STARTING a mediaPlayback foreground service from the
        // background. The first 'playing' always arrives from a foreground user
        // gesture, so gate the first start on it; once running, keep updating
        // (incl. while paused) so the controls stay live.
        if (!playing && !active) return;
        active = true;
        Intent i = new Intent(appContext, MediaPlaybackService.class)
                .setAction(MediaPlaybackService.ACTION_UPDATE)
                .putExtra(MediaPlaybackService.EXTRA_TITLE, title)
                .putExtra(MediaPlaybackService.EXTRA_ARTIST, artist)
                .putExtra(MediaPlaybackService.EXTRA_ALBUM, album)
                .putExtra(MediaPlaybackService.EXTRA_DURATION, durationMs)
                .putExtra(MediaPlaybackService.EXTRA_POSITION, positionMs)
                .putExtra(MediaPlaybackService.EXTRA_RATE, rate)
                .putExtra(MediaPlaybackService.EXTRA_PLAYING, playing);
        ContextCompat.startForegroundService(appContext, i);
    }

    // ── native → JS ──────────────────────────────────────────────────────────────
    static void dispatch(String action) {
        eval("window.__madshareMediaAction&&window.__madshareMediaAction('" + action + "')");
    }

    static void dispatchSeek(long posMs) {
        eval("window.__madshareMediaAction&&window.__madshareMediaAction('seekto'," + posMs + ")");
    }

    private static void eval(final String js) {
        MAIN.post(() -> {
            WebView wv = webViewRef.get();
            if (wv != null) wv.evaluateJavascript(js, null);
        });
    }
}
