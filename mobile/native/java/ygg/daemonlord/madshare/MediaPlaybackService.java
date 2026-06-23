package ygg.daemonlord.madshare;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.os.Build;
import android.os.IBinder;
import android.support.v4.media.MediaMetadataCompat;
import android.support.v4.media.session.MediaSessionCompat;
import android.support.v4.media.session.PlaybackStateCompat;

import androidx.annotation.Nullable;
import androidx.core.app.NotificationCompat;
import androidx.media.app.NotificationCompat.MediaStyle;
import androidx.media.session.MediaButtonReceiver;

/**
 * Foreground media service: owns the {@link MediaSessionCompat}, renders the media
 * notification, and (via the foreground service) keeps the WebView's {@code <audio>}
 * playing while the app is backgrounded. Commands arrive as Intents from
 * {@link MediaBridge}; transport events flow back to the page through MediaBridge's
 * native→JS dispatch.
 */
public class MediaPlaybackService extends Service {
    public static final String ACTION_UPDATE = "ygg.daemonlord.madshare.media.UPDATE";
    public static final String ACTION_STOP   = "ygg.daemonlord.madshare.media.STOP";
    public static final String EXTRA_TITLE    = "title";
    public static final String EXTRA_ARTIST   = "artist";
    public static final String EXTRA_ALBUM    = "album";
    public static final String EXTRA_DURATION = "duration"; // ms
    public static final String EXTRA_POSITION = "position"; // ms
    public static final String EXTRA_RATE     = "rate";
    public static final String EXTRA_PLAYING  = "playing";

    private static final String CHANNEL_ID = "madshare_playback";
    private static final int NOTIF_ID = 1001;

    private MediaSessionCompat session;

    // Last-known snapshot, so MEDIA_BUTTON intents (which must also call
    // startForeground promptly) can rebuild a correct notification.
    private String curTitle = "", curArtist = "";
    private boolean curPlaying = false;

    @Override
    public void onCreate() {
        super.onCreate();
        createChannel();
        session = new MediaSessionCompat(this, "MadshareMedia");
        session.setCallback(new MediaSessionCompat.Callback() {
            @Override public void onPlay()           { MediaBridge.dispatch("play"); }
            @Override public void onPause()          { MediaBridge.dispatch("pause"); }
            @Override public void onSkipToNext()     { MediaBridge.dispatch("nexttrack"); }
            @Override public void onSkipToPrevious() { MediaBridge.dispatch("previoustrack"); }
            @Override public void onSeekTo(long pos) { MediaBridge.dispatchSeek(pos); }
            @Override public void onStop()           { MediaBridge.dispatch("pause"); stopPlayback(); }
        });
        session.setActive(true);
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        final String action = intent == null ? null : intent.getAction();

        if (ACTION_STOP.equals(action)) {
            stopPlayback();
            return START_NOT_STICKY;
        }

        if (ACTION_UPDATE.equals(action)) {
            curTitle  = orEmpty(intent.getStringExtra(EXTRA_TITLE));
            curArtist = orEmpty(intent.getStringExtra(EXTRA_ARTIST));
            curPlaying = intent.getBooleanExtra(EXTRA_PLAYING, false);
            String album = orEmpty(intent.getStringExtra(EXTRA_ALBUM));
            long duration = (long) intent.getDoubleExtra(EXTRA_DURATION, 0);
            long position = (long) intent.getDoubleExtra(EXTRA_POSITION, 0);
            float rate    = (float) intent.getDoubleExtra(EXTRA_RATE, 1.0);

            session.setMetadata(new MediaMetadataCompat.Builder()
                    .putString(MediaMetadataCompat.METADATA_KEY_TITLE, curTitle)
                    .putString(MediaMetadataCompat.METADATA_KEY_ARTIST, curArtist)
                    .putString(MediaMetadataCompat.METADATA_KEY_ALBUM, album)
                    .putLong(MediaMetadataCompat.METADATA_KEY_DURATION, duration)
                    .build());

            long actions = PlaybackStateCompat.ACTION_PLAY_PAUSE
                    | PlaybackStateCompat.ACTION_PLAY | PlaybackStateCompat.ACTION_PAUSE
                    | PlaybackStateCompat.ACTION_SKIP_TO_NEXT | PlaybackStateCompat.ACTION_SKIP_TO_PREVIOUS
                    | PlaybackStateCompat.ACTION_SEEK_TO | PlaybackStateCompat.ACTION_STOP;
            int state = curPlaying ? PlaybackStateCompat.STATE_PLAYING : PlaybackStateCompat.STATE_PAUSED;
            session.setPlaybackState(new PlaybackStateCompat.Builder()
                    .setActions(actions)
                    .setState(state, position, rate)
                    .build());
        }

        // Always (re)enter the foreground promptly — startForegroundService demands a
        // startForeground() within ~5s, including for MEDIA_BUTTON deliveries.
        goForeground();

        // Route hardware / notification media buttons into the session callback.
        if (intent != null && Intent.ACTION_MEDIA_BUTTON.equals(action)) {
            MediaButtonReceiver.handleIntent(session, intent);
        }
        return START_NOT_STICKY;
    }

    private void goForeground() {
        Notification notif = buildNotification();
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            startForeground(NOTIF_ID, notif, ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PLAYBACK);
        } else {
            startForeground(NOTIF_ID, notif);
        }
    }

    private void stopPlayback() {
        session.setActive(false);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
            stopForeground(Service.STOP_FOREGROUND_REMOVE);
        } else {
            stopForeground(true);
        }
        stopSelf();
    }

    private Notification buildNotification() {
        int piFlags = PendingIntent.FLAG_UPDATE_CURRENT
                | (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M ? PendingIntent.FLAG_IMMUTABLE : 0);
        PendingIntent contentIntent = null;
        Intent launch = getPackageManager().getLaunchIntentForPackage(getPackageName());
        if (launch != null) {
            contentIntent = PendingIntent.getActivity(this, 0, launch, piFlags);
        }

        NotificationCompat.Action prev = new NotificationCompat.Action(
                android.R.drawable.ic_media_previous, "Previous",
                MediaButtonReceiver.buildMediaButtonPendingIntent(this, PlaybackStateCompat.ACTION_SKIP_TO_PREVIOUS));
        NotificationCompat.Action next = new NotificationCompat.Action(
                android.R.drawable.ic_media_next, "Next",
                MediaButtonReceiver.buildMediaButtonPendingIntent(this, PlaybackStateCompat.ACTION_SKIP_TO_NEXT));
        NotificationCompat.Action playPause = curPlaying
                ? new NotificationCompat.Action(android.R.drawable.ic_media_pause, "Pause",
                        MediaButtonReceiver.buildMediaButtonPendingIntent(this, PlaybackStateCompat.ACTION_PAUSE))
                : new NotificationCompat.Action(android.R.drawable.ic_media_play, "Play",
                        MediaButtonReceiver.buildMediaButtonPendingIntent(this, PlaybackStateCompat.ACTION_PLAY));

        return new NotificationCompat.Builder(this, CHANNEL_ID)
                .setSmallIcon(getApplicationInfo().icon)
                .setContentTitle(curTitle)
                .setContentText(curArtist)
                .setContentIntent(contentIntent)
                .setVisibility(NotificationCompat.VISIBILITY_PUBLIC)
                .setOngoing(curPlaying)
                .setShowWhen(false)
                .addAction(prev)
                .addAction(playPause)
                .addAction(next)
                .setStyle(new MediaStyle()
                        .setMediaSession(session.getSessionToken())
                        .setShowActionsInCompactView(0, 1, 2))
                .setDeleteIntent(MediaButtonReceiver.buildMediaButtonPendingIntent(this, PlaybackStateCompat.ACTION_STOP))
                .build();
    }

    private void createChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            NotificationManager nm = getSystemService(NotificationManager.class);
            NotificationChannel ch = new NotificationChannel(CHANNEL_ID, "Playback",
                    NotificationManager.IMPORTANCE_LOW);
            ch.setShowBadge(false);
            ch.setLockscreenVisibility(Notification.VISIBILITY_PUBLIC);
            nm.createNotificationChannel(ch);
        }
    }

    private static String orEmpty(String s) { return s == null ? "" : s; }

    @Override
    public void onDestroy() {
        if (session != null) session.release();
        super.onDestroy();
    }

    @Nullable
    @Override
    public IBinder onBind(Intent intent) { return null; }
}
