package media

// Code generated from toolchain/versions.env by
// `go test ./internal/media -run TestSpeechModelsMatchVersionsEnv -update`.
// DO NOT EDIT: change versions.env and regenerate. The test fails when
// they differ, so versions.env stays the one place a digest is written.

// SpeechModel is the pinned whisper model.
var SpeechModel = Model{
	Name:   "large-v3-turbo-q8_0",
	URL:    "https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-large-v3-turbo-q8_0.bin",
	SHA256: "317eb69c11673c9de1e1f0d459b253999804ec71ac4c23c17ecf5fbe24e259a1",
	Size:   874188075,
}

// VADModel is the pinned Silero VAD model.
var VADModel = Model{
	Name:   "silero-v6.2.0",
	URL:    "https://huggingface.co/ggml-org/whisper-vad/resolve/9ffd54a1e1ee413ddf265af9913beaf518d1639b/ggml-silero-v6.2.0.bin",
	SHA256: "2aa269b785eeb53a82983a20501ddf7c1d9c48e33ab63a41391ac6c9f7fb6987",
	Size:   885098,
}
