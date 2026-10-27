// Pure audio policy helpers shared by the meeting UI and dependency-free tests.
(function (root, factory) {
	var api = factory()
	if (typeof module === "object" && module.exports) module.exports = api
	else root.MeetCaptureOptions = api
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
	"use strict"
	var BITRATE = 96000, CHUNK_MS = 15000, ROTATE_MS = 10 * 60 * 1000
	function normalize(options) {
		options = options || {}
		return { language: ["es", "en", "mixed"].indexOf(options.language) >= 0 ? options.language : "es",
			shared: options.shared === true, enabled: options.enabled === true,
			deviceId: typeof options.deviceId === "string" ? options.deviceId : "",
			deviceLabel: typeof options.deviceLabel === "string" ? options.deviceLabel.slice(0, 160) : "" }
	}
	function constraints(deviceId, supported) {
		supported = supported || {}
		var audio = {}
		;["echoCancellation", "noiseSuppression", "autoGainControl"].forEach(function (key) { if (supported[key]) audio[key] = true })
		if (supported.sampleRate) audio.sampleRate = { ideal: 48000 }
		if (supported.channelCount) audio.channelCount = { ideal: 1 }
		if (deviceId && supported.deviceId) audio.deviceId = { exact: deviceId }
		return { audio: audio, video: false }
	}
	function chooseMime(isSupported) {
		return ["audio/webm;codecs=opus", "audio/webm"].find(function (mime) { return isSupported(mime) }) || ""
	}
	function recorderOptions(mime) { return { mimeType: mime, audioBitsPerSecond: BITRATE } }
	function signalStats(samples) {
		var sum = 0, peak = 0, clipped = 0
		for (var i = 0; i < samples.length; i++) {
			var value = Math.abs(samples[i]); sum += value * value
			if (value > peak) peak = value
			if (value >= 0.98) clipped++
		}
		return { rms: samples.length ? Math.sqrt(sum / samples.length) : 0, peak: peak, clipped: clipped, samples: samples.length }
	}
	function signalTest(maxRMS, clipped, samples) {
		if (samples && clipped / samples > 0.01) return "clipping"
		return maxRMS >= 0.005 ? "signal_detected" : "low_signal"
	}
	function metadata(settings, recorder, label, signal) {
		settings = settings || {}; signal = signal || { result: "not_run", detected: false }
		var out = {
			bitrate: Number.isFinite(recorder.audioBitsPerSecond) ? recorder.audioBitsPerSecond : BITRATE,
			mime: String(recorder.mimeType || "").slice(0, 100), device_label: String(label || "").slice(0, 160),
			speech_detected: signal.detected === true,
			signal_test: ["not_run", "signal_detected", "low_signal", "clipping", "unavailable"].indexOf(signal.result) >= 0 ? signal.result : "not_run",
		}
		if (Number.isFinite(settings.sampleRate) && settings.sampleRate > 0) out.sample_rate = settings.sampleRate
		if (Number.isFinite(settings.channelCount) && settings.channelCount > 0) out.channels = settings.channelCount
		return out
	}
	function matchingDevice(devices, label) {
		if (!label) return null
		function normalized(value) { return String(value || "").trim().toLowerCase().replace(/\s+/g, " ") }
		var wanted = normalized(label)
		var matches = (devices || []).filter(function (device) { return normalized(device.label) === wanted })
		return matches.length === 1 ? matches[0] : null
	}
	// enqueue is the capture-wide serial queue. A recorder closes only after its
	// final dataavailable event; its immutable manifest is sent behind those bytes.
	function createStreamUploadSession(transport) {
		var count = 0, bytes = 0, closed = false, completion = null
		return {
			push: function (blob, end) {
				if (closed) throw new Error("audio chunk after recorder closure")
				if (!blob || !blob.size) return
				if (!Number.isSafeInteger(blob.size) || blob.size < 0) throw new Error("invalid audio chunk size")
				var sequence = count++
				bytes += blob.size
				return transport.enqueue(function () {
					if (!transport.isFailed()) return transport.upload(sequence, blob, end)
				})
			},
			finish: function (end) {
				if (closed) return completion
				closed = true
				if (!count) return (completion = Promise.resolve())
				var manifest = Object.freeze({ seq: count, bytes: bytes, end: end })
				completion = transport.enqueue(function () {
					if (!transport.isFailed()) return transport.finalize(manifest)
				})
				return completion
			},
		}
	}
	return { BITRATE: BITRATE, CHUNK_MS: CHUNK_MS, ROTATE_MS: ROTATE_MS, normalize: normalize, constraints: constraints,
		chooseMime: chooseMime, recorderOptions: recorderOptions, signalStats: signalStats, signalTest: signalTest,
		metadata: metadata, matchingDevice: matchingDevice, createStreamUploadSession: createStreamUploadSession }
})
