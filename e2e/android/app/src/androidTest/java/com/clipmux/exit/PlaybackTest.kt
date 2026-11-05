package com.clipmux.exit

import android.os.Handler
import android.os.HandlerThread
import androidx.media3.common.C
import androidx.media3.common.MediaItem
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import androidx.media3.exoplayer.ExoPlayer
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.net.HttpURLConnection
import java.net.URL
import java.util.concurrent.Callable
import java.util.concurrent.FutureTask
import java.util.concurrent.atomic.AtomicReference
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

// ExoPlayer plays ClipMux's signed, AES-128 CMAF HLS with its separate audio
// rendition (docs/plans/P1-core-product.md, exit test).
@RunWith(AndroidJUnit4::class)
class PlaybackTest {
    private val fixture = "http://127.0.0.1:8099"

    private fun get(url: String): Pair<Int, String> {
        val c = URL(url).openConnection() as HttpURLConnection
        val code = c.responseCode
        val body = (if (code < 400) c.inputStream else c.errorStream)?.bufferedReader()?.readText().orEmpty()
        c.disconnect()
        return code to body
    }

    @Test
    fun exoPlayerPlaysTheSignedEncryptedVideoWithSeparateAudio() {
        val cfg = JSONObject(get("$fixture/config").second)
        val base = cfg.getString("base_url")
        val pid = cfg.getString("playback_id")
        val token = get("$fixture/token?ttl=3600").second
        val context = InstrumentationRegistry.getInstrumentation().targetContext

        val thread = HandlerThread("player").apply { start() }
        val handler = Handler(thread.looper)
        fun <T> onPlayer(block: () -> T): T = FutureTask(Callable(block)).also { handler.post(it) }.get()

        val error = AtomicReference<PlaybackException?>(null)
        val player = onPlayer {
            ExoPlayer.Builder(context).setLooper(thread.looper).build().apply {
                addListener(object : Player.Listener {
                    override fun onPlayerError(e: PlaybackException) {
                        error.set(e)
                    }
                })
                volume = 0f
                setMediaItem(MediaItem.fromUri("$base/v/$token/$pid/master.m3u8"))
                prepare()
                play()
            }
        }
        try {
            val deadline = System.currentTimeMillis() + 120_000
            while (onPlayer { player.currentPosition } < 8_000 && error.get() == null && System.currentTimeMillis() < deadline) {
                Thread.sleep(500)
            }
            assertNull("playback error: ${error.get()}", error.get())
            assertTrue("position ${onPlayer { player.currentPosition }} ms, want past 8 s", onPlayer { player.currentPosition } >= 8_000)
            val (video, audio) = onPlayer {
                val groups = player.currentTracks.groups
                groups.any { it.type == C.TRACK_TYPE_VIDEO && it.isSelected } to
                    groups.any { it.type == C.TRACK_TYPE_AUDIO && it.isSelected }
            }
            assertTrue("a video track is selected", video)
            assertTrue("the separate audio rendition is selected", audio)
        } finally {
            onPlayer { player.release() }
            thread.quitSafely()
        }

        // A thumbnail token can't play.
        val thumb = get("$fixture/token?aud=t").second
        assertEquals(403, get("$base/v/$thumb/$pid/master.m3u8").first)
    }
}
