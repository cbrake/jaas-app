// Meeting page: values come from data-* attributes (no inline script, no
// template interpolation inside JavaScript).
;(function () {
	var el = document.getElementById("meet")
	var d = el.dataset
	var api = new JitsiMeetExternalAPI("8x8.vc", {
		roomName: d.appId + "/" + d.room,
		jwt: d.jwt,
		parentNode: el,
		userInfo: { displayName: d.name },
	})
	el.removeAttribute("data-jwt")

	function showEnded() {
		var msg = document.createElement("div")
		msg.className = "ended"
		msg.textContent = "The meeting has ended."
		document.body.replaceChildren(msg)
	}

	var stopCapture = function () {}

	if (d.moderator === "true") {
		var endURL = "/m/" + encodeURIComponent(d.room) + "/end"
		var body = new URLSearchParams({ csrf_token: d.csrf })
		var end = function () {
			navigator.sendBeacon(endURL, body)
		}
		api.addEventListener("readyToClose", function () {
			stopCapture()
			end()
			showEnded()
		})
		window.addEventListener("pagehide", end)
	} else {
		api.addEventListener("readyToClose", function () {
			stopCapture()
			showEnded()
		})
	}

	if (d.capture === "true") {
		api.addEventListener("videoConferenceJoined", function () {
			stopCapture = startCapture()
		})
	}

	// startCapture records this participant's own microphone for the meeting
	// transcript. Nothing is recorded or uploaded while the participant is
	// muted in the meeting: muting closes the current audio stream and
	// unmuting opens a new one (each stream carries its own start time, so
	// the transcript stays aligned). A stream is also rotated every 60 min to
	// stay under the speech-to-text file limit. Chunks upload every 15 s.
	function startCapture() {
		var badge = document.createElement("div")
		badge.className = "capture-badge"
		badge.setAttribute("role", "status")
		document.body.appendChild(badge)
		var say = function (text, cls) {
			badge.textContent = text
			badge.className = "capture-badge" + (cls ? " " + cls : "")
		}
		var mime = "audio/webm;codecs=opus"
		if (
			!window.MediaRecorder ||
			!navigator.mediaDevices ||
			!MediaRecorder.isTypeSupported(mime)
		) {
			say("This browser cannot transcribe your microphone.", "off")
			return function () {}
		}
		var stopped = false
		var media = null
		var current = null // { rec, rotate }
		var failed = false
		var queue = Promise.resolve()
		var wait = function (ms) {
			return new Promise(function (r) {
				setTimeout(r, ms)
			})
		}
		var newStreamId = function () {
			var id = new Uint8Array(18)
			crypto.getRandomValues(id)
			return btoa(String.fromCharCode.apply(null, id))
				.replace(/\+/g, "-")
				.replace(/\//g, "_")
				.replace(/=+$/, "")
		}
		var upload = function (url, blob, attempt) {
			return fetch(url, {
				method: "POST",
				headers: { "Content-Type": "audio/webm", "X-CSRF-Token": d.csrf },
				body: blob,
				credentials: "same-origin",
				keepalive: blob.size < 60000,
			})
				.then(function (r) {
					if (r.ok) return
					if (r.status >= 500 && attempt < 3)
						return wait(2000 * (attempt + 1)).then(function () {
							return upload(url, blob, attempt + 1)
						})
					throw new Error("upload " + r.status)
				})
				.catch(function (err) {
					if (attempt < 3 && !(err && /upload/.test(err.message))) {
						return wait(2000 * (attempt + 1)).then(function () {
							return upload(url, blob, attempt + 1)
						})
					}
					failed = true
					closeStream()
					say("Transcription of your microphone stopped.", "off")
				})
		}
		var openStream = function () {
			if (stopped || failed || current || !media) return
			var base =
				"/m/" +
				encodeURIComponent(d.room) +
				"/audio?stream=" +
				newStreamId() +
				"&start=" +
				Date.now() +
				"&seq="
			var seq = 0
			var rec = new MediaRecorder(media, {
				mimeType: mime,
				audioBitsPerSecond: 16000,
			})
			rec.ondataavailable = function (ev) {
				if (failed || !ev.data || !ev.data.size) return
				var url = base + seq++
				var blob = ev.data
				queue = queue.then(function () {
					return upload(url, blob, 0)
				})
			}
			rec.start(15000)
			current = {
				rec: rec,
				rotate: setTimeout(
					function () {
						closeStream()
						openStream()
					},
					60 * 60 * 1000
				),
			}
			say("● Transcribing your microphone")
		}
		var closeStream = function () {
			if (!current) return
			clearTimeout(current.rotate)
			if (current.rec.state !== "inactive") current.rec.stop()
			current = null
		}
		var cleanup = function () {
			stopped = true
			closeStream()
			if (media)
				media.getTracks().forEach(function (t) {
					t.stop()
				})
			badge.remove()
		}
		say("Asking for microphone permission for the transcript…")
		navigator.mediaDevices
			.getUserMedia({
				audio: {
					echoCancellation: true,
					noiseSuppression: true,
					autoGainControl: true,
					channelCount: 1,
				},
			})
			.then(function (stream) {
				media = stream
				if (stopped) {
					cleanup()
					return
				}
				var setMuted = function (muted) {
					if (failed) return
					if (muted) {
						closeStream()
						say(
							"Microphone muted: nothing is recorded or transcribed.",
							"muted"
						)
					} else {
						openStream()
					}
				}
				api.isAudioMuted().then(setMuted)
				api.addEventListener("audioMuteStatusChanged", function (e) {
					setMuted(e.muted)
				})
				window.addEventListener("pagehide", cleanup)
			})
			.catch(function () {
				say("Transcription off: microphone permission was not granted.", "off")
			})
		return function () {
			cleanup()
		}
	}
})()
