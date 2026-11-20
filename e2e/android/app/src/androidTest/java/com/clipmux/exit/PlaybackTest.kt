package com.clipmux.exit

import android.os.Handler
import android.os.HandlerThread
import android.os.Looper
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
// rendition (docs/plans/P1-core-product.md, exit test) and shows its
// subtitles track (P6-T03).
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

    // A player on a looper thread of its own: `on` runs a block there and
    // returns its result; `release` releases the player and ends the thread.
    private class PlayerThread(build: (Looper) -> ExoPlayer) {
        private val thread = HandlerThread("player").apply { start() }
        private val handler = Handler(thread.looper)
        val player: ExoPlayer = on { build(thread.looper) }

        fun <T> on(block: () -> T): T = FutureTask(Callable(block)).also { handler.post(it) }.get()

        fun release() {
            on { player.release() }
            thread.quitSafely()
        }
    }

    private fun play(url: String, listener: Player.Listener, configure: ExoPlayer.() -> Unit = {}): PlayerThread {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        return PlayerThread { looper ->
            ExoPlayer.Builder(context).setLooper(looper).build().apply {
                addListener(listener)
                volume = 0f
                configure()
                setMediaItem(MediaItem.fromUri(url))
                prepare()
                play()
            }
        }
    }

    private fun signedMaster(): String {
        val cfg = JSONObject(get("$fixture/config").second)
        val token = get("$fixture/token?ttl=3600").second
        return "${cfg.getString("base_url")}/v/$token/${cfg.getString("playback_id")}/master.m3u8"
    }

    @Test
    fun exoPlayerPlaysTheSignedEncryptedVideoWithSeparateAudio() {
        val cfg = JSONObject(get("$fixture/config").second)
        val base = cfg.getString("base_url")
        val pid = cfg.getString("playback_id")
        val error = AtomicReference<PlaybackException?>(null)
        val p = play(
            signedMaster(),
            object : Player.Listener {
                override fun onPlayerError(e: PlaybackException) {
                    error.set(e)
                }
            },
        )
        try {
            val deadline = System.currentTimeMillis() + 120_000
            while (p.on { p.player.currentPosition } < 8_000 && error.get() == null && System.currentTimeMillis() < deadline) {
                Thread.sleep(500)
            }
            assertNull("playback error: ${error.get()}", error.get())
            assertTrue("position ${p.on { p.player.currentPosition }} ms, want past 8 s", p.on { p.player.currentPosition } >= 8_000)
            val (video, audio) = p.on {
                val groups = p.player.currentTracks.groups
                groups.any { it.type == C.TRACK_TYPE_VIDEO && it.isSelected } to
                    groups.any { it.type == C.TRACK_TYPE_AUDIO && it.isSelected }
            }
            assertTrue("a video track is selected", video)
            assertTrue("the separate audio rendition is selected", audio)
        } finally {
            p.release()
        }

        // A thumbnail token can't play.
        val thumb = get("$fixture/token?aud=t").second
        assertEquals(403, get("$base/v/$thumb/$pid/master.m3u8").first)
    }

    // The subtitles track from the master, a single-segment WebVTT playlist as
    // long as the video (P6-T03, P5 open question 8): the cue the fixture's
    // fake speech API heard at 1–3 s is shown within ±0.5 s of its time, as
    // hls.js shows it in e2e/web/tests/tracks.spec.ts (P5 check 14).
    @Test
    fun exoPlayerShowsTheSubtitleCueAtItsTime() {
        val cfg = JSONObject(get("$fixture/config").second)
        val cueText = cfg.getString("cue_text")
        val cueStartMs = (cfg.getDouble("cue_start") * 1000).toLong()

        data class Seen(val text: String, val presentationMs: Long, val positionMs: Long)
        val seen = AtomicReference<Seen?>(null)
        val error = AtomicReference<PlaybackException?>(null)
        val listener = object : Player.Listener {
            override fun onPlayerError(e: PlaybackException) {
                error.set(e)
            }

            // On the player's looper, so the position is the one the cue showed at.
            override fun onEvents(player: Player, events: Player.Events) {
                if (!events.contains(Player.EVENT_CUES) || seen.get() != null) return
                val group = player.currentCues
                val text = group.cues.mapNotNull { it.text?.toString() }.joinToString("\n").trim()
                if (text.isNotEmpty()) seen.set(Seen(text, group.presentationTimeUs / 1000, player.currentPosition))
            }
        }
        // The master lists the track AUTOSELECT=YES, DEFAULT=NO, so ExoPlayer
        // shows it only when the viewer's language asks for it.
        val p = play(signedMaster(), listener) {
            trackSelectionParameters = trackSelectionParameters.buildUpon().setPreferredTextLanguage("en").build()
        }
        try {
            val deadline = System.currentTimeMillis() + 120_000
            while (seen.get() == null && error.get() == null && System.currentTimeMillis() < deadline) {
                Thread.sleep(100)
            }
            assertNull("playback error: ${error.get()}", error.get())
            val text = p.on {
                p.player.currentTracks.groups.filter { it.type == C.TRACK_TYPE_TEXT && it.isSelected }
                    .map { g -> (0 until g.length).filter { g.isTrackSelected(it) }.map { g.getTrackFormat(it) } }
                    .flatten().map { "${it.sampleMimeType}:${it.language}:${it.label}" }
            }
            val s = seen.get()
                ?: throw AssertionError("no cue shown within 120 s; selected text tracks $text, position ${p.on { p.player.currentPosition }} ms")
            assertTrue("selected text tracks $text, want one in English", text.any { it.contains(":en:") })
            assertEquals(cueText, s.text)
            assertTrue("cue presented at ${s.presentationMs} ms, want $cueStartMs ± 500 ms", Math.abs(s.presentationMs - cueStartMs) <= 500)
            // The cue runs 1–3 s; it shows while the playhead is there.
            assertTrue("cue shown at position ${s.positionMs} ms, want ${cueStartMs - 500}–3500 ms", s.positionMs in (cueStartMs - 500)..3_500)
        } finally {
            p.release()
        }
    }
}
