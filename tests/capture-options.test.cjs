const test = require("node:test")
const assert = require("node:assert/strict")
const policy = require("../static/capture-options.js")

test("capture is opt-in; ES is default and mixed remains explicit", () => {
	assert.deepEqual(policy.normalize(), { language: "es", shared: false, enabled: false, deviceId: "", deviceLabel: "" })
	assert.equal(policy.normalize({ language: "mixed", shared: true, enabled: true }).language, "mixed")
	assert.equal(policy.normalize({ language: "unexpected" }).language, "es")
	assert.equal(policy.normalize({ enabled: "true" }).enabled, false)
})

test("constraints request 48 kHz mono only when supported", () => {
	const supported = { sampleRate: true, channelCount: true, deviceId: true, echoCancellation: true, noiseSuppression: true, autoGainControl: true }
	assert.deepEqual(policy.constraints("host-origin-device", supported), { audio: {
		echoCancellation: true, noiseSuppression: true, autoGainControl: true,
		sampleRate: { ideal: 48000 }, channelCount: { ideal: 1 }, deviceId: { exact: "host-origin-device" },
	}, video: false })
	assert.deepEqual(policy.constraints("host-origin-device", {}), { audio: {}, video: false })
})

test("only supported WebM MIME is selected, without pretending to transcode", () => {
	assert.equal(policy.chooseMime(mime => mime === "audio/webm;codecs=opus"), "audio/webm;codecs=opus")
	assert.equal(policy.chooseMime(mime => mime === "audio/webm"), "audio/webm")
	assert.equal(policy.chooseMime(() => false), "")
	assert.deepEqual(policy.recorderOptions("audio/webm;codecs=opus"), { mimeType: "audio/webm;codecs=opus", audioBitsPerSecond: 96000 })
	assert.equal(policy.CHUNK_MS, 15000)
	assert.equal(policy.ROTATE_MS, 600000)
	assert.equal(policy.BITRATE * policy.ROTATE_MS / 1000 / 8, 7200000)
})

test("RMS observations do not classify language, identity or speech", () => {
	assert.deepEqual(policy.signalStats(new Float32Array(4)), { rms: 0, peak: 0, clipped: 0, samples: 4 })
	const measured = policy.signalStats(new Float32Array([0.5, -0.5]))
	assert.equal(measured.rms, 0.5)
	assert.equal(policy.signalTest(0, 0, 100), "low_signal")
	assert.equal(policy.signalTest(0.01, 0, 100), "signal_detected")
	assert.equal(policy.signalTest(0.1, 2, 100), "clipping")
})

test("metadata preserves measured settings, omits unknown sample rate and raw device id", () => {
	const unknown = policy.metadata({ deviceId: "private-origin-id" }, { mimeType: "audio/webm" }, "Desk microphone")
	assert.equal(unknown.sample_rate, undefined)
	assert.equal(unknown.channels, undefined)
	assert.equal(unknown.deviceId, undefined)
	assert.equal(unknown.signal_test, "not_run")
	assert.equal(unknown.speech_detected, false)
	const measured = policy.metadata({ sampleRate: 44100, channelCount: 2 }, { mimeType: "audio/webm;codecs=opus", audioBitsPerSecond: 97000 },
		"x".repeat(200), { result: "low_signal", detected: false })
	assert.equal(measured.sample_rate, 44100)
	assert.equal(measured.channels, 2)
	assert.equal(measured.bitrate, 97000)
	assert.equal(measured.device_label.length, 160)
	assert.equal(measured.signal_test, "low_signal")
})

test("Jitsi device mapping uses a unique matching label, not the host device id", () => {
	const devices = [{ deviceId: "iframe-id", label: "  USB   Microphone " }, { deviceId: "other-id", label: "Internal" }]
	assert.equal(policy.matchingDevice(devices, "usb microphone").deviceId, "iframe-id")
	assert.equal(policy.matchingDevice(devices, "host-origin-device"), null)
	assert.equal(policy.matchingDevice(devices.concat({ deviceId: "duplicate", label: "USB Microphone" }), "USB Microphone"), null)
})


test("finalization waits for every queued chunk and freezes the exact manifest", async () => {
	let tail = Promise.resolve(), release
	const first = new Promise(resolve => { release = resolve })
	const events = []
	const session = policy.createStreamUploadSession({
		enqueue: task => (tail = tail.then(task)),
		isFailed: () => false,
		upload: async (seq, blob, end) => {
			events.push(["upload", seq, blob.size, end])
			if (seq === 0) await first
		},
		finalize: manifest => { events.push(["finalize", manifest]); assert.equal(Object.isFrozen(manifest), true) },
	})
	session.push({ size: 3 }, 1000)
	session.push({ size: 7 }, 2000)
	const completion = session.finish(2500)
	assert.equal(session.finish(9999), completion)
	await Promise.resolve()
	assert.deepEqual(events, [["upload", 0, 3, 1000]])
	release()
	await completion
	assert.deepEqual(events, [["upload", 0, 3, 1000], ["upload", 1, 7, 2000], ["finalize", { seq: 2, bytes: 10, end: 2500 }]])
	assert.throws(() => session.push({ size: 1 }, 3000), /closure/)
})

test("failed uploads and empty recorders cannot finalize", async () => {
	let tail = Promise.resolve(), failed = false
	const events = []
	const transport = {
		enqueue: task => (tail = tail.then(task)),
		isFailed: () => failed,
		upload: () => { failed = true; events.push("failed upload") },
		finalize: () => { events.push("finalize") },
	}
	const session = policy.createStreamUploadSession(transport)
	session.push({ size: 3 }, 1000)
	session.push({ size: 5 }, 2000)
	await session.finish(2000)
	assert.deepEqual(events, ["failed upload"])
	failed = false
	const empty = policy.createStreamUploadSession(transport)
	empty.push({ size: 0 }, 3000)
	await empty.finish(3000)
	assert.deepEqual(events, ["failed upload"])
})

// Exercise the actual meeting lifecycle without microphones, network or Jitsi.
// In particular, mute clears current before MediaRecorder's final async events.
async function runMockMeeting({ uploadFailure = false, loseFirstFinalize = false, finalBytes = 7 } = {}) {
	const fs = require("node:fs")
	const path = require("node:path")
	const vm = require("node:vm")
	const { webcrypto } = require("node:crypto")
	const elements = new Map(), events = [], timers = new Set(), recorders = []
	function element(id) {
		return { id, dataset: {}, value: "", checked: false, disabled: false, hidden: false, textContent: "",
			handlers: {}, classList: { add() {} }, options: [{ text: "Default" }], selectedIndex: 0,
			addEventListener(name, fn) { this.handlers[name] = fn },
			setAttribute() {}, removeAttribute() {}, replaceChildren() {}, appendChild() {}, remove() {}, focus() {},
		}
	}
	;["meet", "audio-setup", "meeting-language", "shared-microphone", "audio-device", "audio-detect", "audio-test",
		"audio-test-status", "audio-device-status", "audio-level", "capture-opt-in", "join-meeting",
		"audio-settings-toggle", "capture-explanation"].forEach(id => elements.set(id, element(id)))
	elements.get("meet").dataset = { appId: "mock", room: "test-room", jwt: "mock-jwt", name: "Test", moderator: "true", csrf: "mock-csrf", capture: "true" }
	elements.get("meeting-language").value = "es"
	elements.get("capture-opt-in").checked = true
	let api, finalizeAttempts = 0
	class MockJitsi {
		constructor() { this.handlers = {}; api = this }
		addEventListener(name, fn) { this.handlers[name] = fn }
		removeEventListener(name) { delete this.handlers[name] }
		isAudioMuted() { return Promise.resolve(false) }
		getAvailableDevices() { return Promise.resolve({ audioInput: [] }) }
	}
	class MockRecorder {
		static isTypeSupported() { return true }
		constructor() { this.state = "inactive"; this.mimeType = "audio/webm;codecs=opus"; this.audioBitsPerSecond = 96000; recorders.push(this) }
		start() { this.state = "recording" }
		stop() {
			this.state = "inactive"
			setImmediate(() => {
				if (this.ondataavailable) this.ondataavailable({ data: { size: finalBytes } })
				if (this.onstop) this.onstop()
			})
		}
	}
	const track = { label: "Mock microphone", stop() {}, getSettings() { return { sampleRate: 48000, channelCount: 1 } }, addEventListener() {} }
	const media = { getTracks: () => [track], getAudioTracks: () => [track] }
	const context = {
		document: { getElementById: id => elements.get(id), createElement: element, body: element("body") },
		navigator: { mediaDevices: { getUserMedia: () => Promise.resolve(media), getSupportedConstraints: () => ({}), addEventListener() {} },
			sendBeacon(url) { events.push({ kind: "end", url }); return true } },
		JitsiMeetExternalAPI: MockJitsi, MediaRecorder: MockRecorder, MeetCaptureOptions: policy,
		URLSearchParams, crypto: webcrypto, Uint8Array, btoa: value => Buffer.from(value, "binary").toString("base64"),
		addEventListener() {},
		setTimeout(fn, ms) { const timer = setTimeout(fn, ms <= 12000 ? 0 : ms); timer.unref(); timers.add(timer); return timer },
		clearTimeout(timer) { clearTimeout(timer); timers.delete(timer) },
		fetch: async (url, request) => {
			assert.equal(request.method, "POST")
			assert.equal(request.headers["X-CSRF-Token"], "mock-csrf")
			const finalizing = url.includes("/audio/finalize?")
			events.push({ kind: finalizing ? "finalize" : "upload", url, body: request.body })
			if (finalizing && loseFirstFinalize && finalizeAttempts++ === 0) throw new Error("response lost")
			return { ok: finalizing || !uploadFailure, status: finalizing || !uploadFailure ? 204 : 503 }
		},
	}
	context.window = context
	vm.runInNewContext(fs.readFileSync(path.join(__dirname, "../static/meeting.js"), "utf8"), context)
	elements.get("join-meeting").handlers.click()
	api.handlers.videoConferenceJoined()
	for (let i = 0; i < 10 && !recorders.length; i++) await new Promise(setImmediate)
	assert.equal(recorders.length, 1, "capture should have started")
	api.handlers.audioMuteStatusChanged({ muted: true })
	// No final dataavailable/onstop event has happened yet.
	api.handlers.readyToClose()
	for (let i = 0; i < 100 && !events.some(event => event.kind === "end"); i++) await new Promise(resolve => setTimeout(resolve, 5))
	for (const timer of timers) clearTimeout(timer)
	assert.equal(events.at(-1)?.kind, "end", "meeting end must wait for closure/upload result")
	return events
}

test("mute followed immediately by meeting end preserves final blob then retries identical finalize", async () => {
	const events = await runMockMeeting({ loseFirstFinalize: true })
	assert.deepEqual(events.map(event => event.kind), ["upload", "finalize", "finalize", "end"])
	const uploaded = new URL(events[0].url, "https://local.invalid")
	const finalized = new URL(events[1].url, "https://local.invalid")
	assert.equal(finalized.searchParams.get("stream"), uploaded.searchParams.get("stream"))
	assert.equal(finalized.searchParams.get("seq"), "1")
	assert.equal(finalized.searchParams.get("bytes"), "7")
	assert.equal(events[1].url, events[2].url, "finalize retry must retain timestamp and manifest")
	assert.ok(Number(finalized.searchParams.get("end")) >= Number(uploaded.searchParams.get("end")))
})

test("upload exhaustion never submits a finalize manifest", async () => {
	const events = await runMockMeeting({ uploadFailure: true })
	assert.equal(events.filter(event => event.kind === "upload").length, 4)
	assert.equal(events.filter(event => event.kind === "finalize").length, 0)
})

test("stopping a zero-chunk recorder does not finalize an empty stream", async () => {
	const events = await runMockMeeting({ finalBytes: 0 })
	assert.deepEqual(events.map(event => event.kind), ["end"])
})
