// Meeting values arrive through data attributes. Capture is opt-in and no AI
// request is made here: the host approves processing later in the admin.
(function () {
	"use strict"
	var el = document.getElementById("meet"), d = el.dataset
	var policy = window.MeetCaptureOptions
	var panel = document.getElementById("audio-setup")
	var language = document.getElementById("meeting-language")
	var shared = document.getElementById("shared-microphone")
	var device = document.getElementById("audio-device")
	var detect = document.getElementById("audio-detect")
	var test = document.getElementById("audio-test")
	var testStatus = document.getElementById("audio-test-status")
	var deviceStatus = document.getElementById("audio-device-status")
	var meter = document.getElementById("audio-level")
	var optIn = document.getElementById("capture-opt-in")
	var join = document.getElementById("join-meeting")
	var toggle = document.getElementById("audio-settings-toggle")
	var api = null, capture = null, started = false, ending = false, ended = false
	var testCleanup = function () {}
	var signal = { result: "not_run", detected: false }
	var selectedLabel = "", deviceTimer = null
	var mediaDevices = navigator.mediaDevices
	var captureSupported = !!(mediaDevices && window.MediaRecorder && policy.chooseMime(MediaRecorder.isTypeSupported.bind(MediaRecorder)))

	if (d.capture !== "true" || !captureSupported) {
		optIn.disabled = true
		document.getElementById("capture-explanation").textContent = d.capture !== "true"
			? "La captura de audio no está habilitada para esta sala."
			: "Este navegador no admite la captura WebM. Puedes entrar sin capturar audio."
	}
	if (!mediaDevices || !mediaDevices.getUserMedia) {
		detect.disabled = test.disabled = true
		deviceStatus.textContent = "Este navegador no permite comprobar el micrófono aquí. Revisa el dispositivo dentro de Jitsi."
	}
	function constraints() {
		return policy.constraints(device.value, mediaDevices.getSupportedConstraints ? mediaDevices.getSupportedConstraints() : {})
	}
	function stopTracks(stream) { if (stream) stream.getTracks().forEach(function (track) { track.stop() }) }
	function resetSignal() {
		signal = { result: "not_run", detected: false }
		meter.value = 0
		testStatus.textContent = started ? "La prueba local se realiza antes de entrar. Revisa la señal y el dispositivo en Jitsi." : "La prueba es local: no guarda, sube ni envía audio a IA."
	}
	function listDevices() {
		return mediaDevices.enumerateDevices().then(function (devices) {
			var previous = device.value
			var inputs = devices.filter(function (item) { return item.kind === "audioinput" })
			device.replaceChildren(new Option("Predeterminado del sistema", ""))
			inputs.forEach(function (item, index) { device.add(new Option(item.label || "Micrófono " + (index + 1), item.deviceId)) })
			var retained = inputs.some(function (item) { return item.deviceId === previous })
			device.value = retained ? previous : ""
			selectedLabel = device.value ? device.options[device.selectedIndex].text : ""
			if (previous && !retained) {
				resetSignal()
				deviceStatus.textContent = "El micrófono anterior se desconectó. Se usará el predeterminado; verifica su nombre."
				if (capture) capture.changeDevice("", "")
			}
			return inputs
		})
	}
	function alignJitsi(label) {
		if (!api || !label) return Promise.resolve()
		return api.getAvailableDevices().then(function (available) {
			var match = policy.matchingDevice(available.audioInput, label)
			if (!match) {
				deviceStatus.textContent = "Captura: " + label + ". Selecciona y verifica el mismo micrófono dentro de Jitsi; no se pudo emparejar por nombre."
				return
			}
			// This ID belongs to the iframe's own device list, never the host page.
			return Promise.resolve(api.setAudioInputDevice(match.label, match.deviceId)).then(function () {
				return api.getCurrentDevices()
			}).then(function (current) {
				if (current.audioInput && policy.matchingDevice([current.audioInput], label)) {
					deviceStatus.textContent = "Micrófono comprobado por nombre en captura y Jitsi: " + label + "."
				} else {
					deviceStatus.textContent = "Captura: " + label + ". Verifica el dispositivo activo en Jitsi."
				}
			})
		}).catch(function () { deviceStatus.textContent = "Captura: " + label + ". No se pudo verificar Jitsi; revisa allí el micrófono activo." })
	}
	detect.addEventListener("click", function () {
		detect.disabled = true
		// User gesture grants labels. This stream is never recorded or uploaded.
		mediaDevices.getUserMedia(constraints()).then(function (stream) {
			stopTracks(stream)
			return listDevices()
		}).then(function () {
			if (!started) deviceStatus.textContent = "Elige un micrófono de la lista. Su nombre se verificará en Jitsi al entrar."
		}).catch(function () {
			deviceStatus.textContent = "No se pudo abrir el micrófono. Revisa los permisos del navegador y vuelve a intentar."
		}).finally(function () { detect.disabled = false })
	})
	device.addEventListener("change", function () {
		selectedLabel = device.value ? device.options[device.selectedIndex].text : ""
		resetSignal()
		if (capture) capture.changeDevice(device.value, selectedLabel)
		else if (started) alignJitsi(selectedLabel)
	})
	test.addEventListener("click", function () {
		if (started) return
		var audioContext = null, stream = null, frame = null, timer = null
		var finished = false, maxRMS = 0, clipped = 0, samples = 0
		test.disabled = detect.disabled = join.disabled = device.disabled = true
		testStatus.textContent = "Di una frase a tu distancia habitual. Sólo medimos nivel local durante 4 segundos."
		function cleanup() {
			if (finished) return
			finished = true
			clearTimeout(timer)
			if (frame !== null) cancelAnimationFrame(frame)
			stopTracks(stream)
			if (audioContext) audioContext.close().catch(function () {})
			test.disabled = detect.disabled = join.disabled = device.disabled = false
			meter.value = 0
		}
		testCleanup = cleanup
		// Create/resume while still inside the user's click gesture. Bound the
		// permission/resume wait so a suspended context cannot lock the controls.
		var AudioContext = window.AudioContext || window.webkitAudioContext
		if (!AudioContext) { cleanup(); testStatus.textContent = "Prueba no disponible en este navegador."; return }
		try { audioContext = new AudioContext() } catch (_) { cleanup(); return }
		var resumed = audioContext.resume()
		resumed.catch(function () {})
		timer = setTimeout(function () { cleanup(); testStatus.textContent = "La prueba agotó el tiempo de espera. Puedes repetirla o entrar sin captura." }, 15000)
		mediaDevices.getUserMedia(constraints()).then(function (input) {
			stream = input
			if (finished) { stopTracks(stream); return }
			return resumed.then(function () {
				if (finished) return
				clearTimeout(timer)
				var source = audioContext.createMediaStreamSource(stream)
				var analyser = audioContext.createAnalyser()
				analyser.fftSize = 2048
				source.connect(analyser) // Deliberately not connected to speakers.
				var data = new Float32Array(analyser.fftSize)
				function measure() {
					if (finished) return
					analyser.getFloatTimeDomainData(data)
					var observation = policy.signalStats(data)
					maxRMS = Math.max(maxRMS, observation.rms)
					clipped += observation.clipped; samples += observation.samples
					meter.value = Math.min(1, observation.rms * 4)
					frame = requestAnimationFrame(measure)
				}
				measure()
				timer = setTimeout(function () {
					var result = policy.signalTest(maxRMS, clipped, samples)
					signal = { result: result, detected: maxRMS >= 0.005 }
					cleanup()
					testStatus.textContent = result === "clipping"
						? "El nivel llega al límite. Baja la ganancia o aléjate un poco y repite la prueba."
						: result === "low_signal"
							? "La señal fue baja. Acércate o revisa la entrada. Puedes continuar: esto no determina si hubo voz."
							: "Se detectó señal de entrada. No es una evaluación de voz ni de precisión de transcripción."
				}, 4000)
			})
		}).catch(function () {
			cleanup()
			signal = { result: "unavailable", detected: false }
			testStatus.textContent = "No se pudo completar la prueba. Revisa permisos y micrófono; puedes entrar sin captura."
		})
	})
	function setPanelVisible(visible) {
		panel.hidden = !visible
		toggle.setAttribute("aria-expanded", String(visible))
		if (visible) device.focus()
		else toggle.focus()
	}
	toggle.addEventListener("click", function () { setPanelVisible(panel.hidden) })
	function sendEnd() {
		if (ended || !started || d.moderator !== "true") return
		ended = true
		navigator.sendBeacon("/m/" + encodeURIComponent(d.room) + "/end", new URLSearchParams({ csrf_token: d.csrf }))
	}
	function showEnded(message) {
		var msg = document.createElement("div")
		msg.className = "ended"
		msg.setAttribute("role", "status")
		msg.textContent = message
		document.body.replaceChildren(msg)
	}
	function finishMeeting() {
		if (ending) return
		ending = true
		testCleanup()
		showEnded(capture ? "Guardando los últimos fragmentos de audio. Mantén esta pestaña abierta..." : "La reunión terminó.")
		Promise.resolve(capture ? capture.stop() : true).then(function (saved) {
			sendEnd()
			showEnded(!capture ? "La reunión terminó." : saved ? "La reunión terminó. Audio recibido para revisión; el procesamiento requiere aprobación." : "La reunión terminó. No se pudo confirmar toda la subida de audio; revisa la captura en administración.")
		})
	}
	join.addEventListener("click", function () {
		if (started) { setPanelVisible(false); return }
		if (typeof JitsiMeetExternalAPI !== "function") {
			deviceStatus.textContent = "No se pudo cargar Jitsi. Recarga la página para volver a intentar."
			return
		}
		var options = policy.normalize({ language: language.value, shared: shared.checked,
			enabled: !optIn.disabled && optIn.checked, deviceId: device.value, deviceLabel: selectedLabel })
		try {
			el.hidden = false
			api = new JitsiMeetExternalAPI("8x8.vc", {
				roomName: d.appId + "/" + d.room, jwt: d.jwt, parentNode: el,
				userInfo: { displayName: d.name },
			})
		} catch (err) {
			el.hidden = true
			deviceStatus.textContent = "No se pudo abrir la reunión. Puedes volver a intentarlo."
			return
		}
		started = true
		el.removeAttribute("data-jwt")
		el.hidden = false
		panel.classList.add("during-meeting")
		language.disabled = shared.disabled = optIn.disabled = test.disabled = true
		join.textContent = "Volver a la reunión"
		toggle.hidden = false
		setPanelVisible(false)
		api.addEventListener("readyToClose", finishMeeting)
		api.addEventListener("videoConferenceJoined", function () {
			alignJitsi(options.deviceLabel)
			if (options.enabled && !capture && !ending) capture = startCapture(options)
		})
	})
	if (mediaDevices && mediaDevices.addEventListener) {
		mediaDevices.addEventListener("devicechange", function () {
			clearTimeout(deviceTimer)
			deviceTimer = setTimeout(function () {
				listDevices().catch(function () { deviceStatus.textContent = "Cambió un dispositivo. Verifica el micrófono seleccionado." })
			}, 250)
		})
	}
	window.addEventListener("pagehide", function () {
		testCleanup()
		if (capture) capture.stop()
		sendEnd()
	})

	function startCapture(initialOptions) {
		var options = policy.normalize(initialOptions)
		var badge = document.createElement("div")
		badge.className = "capture-badge"; badge.setAttribute("role", "status")
		document.body.appendChild(badge)
		function say(text, style) { badge.textContent = text; badge.className = "capture-badge" + (style ? " " + style : "") }
		var mime = policy.chooseMime(MediaRecorder.isTypeSupported.bind(MediaRecorder))
		var stopped = false, failed = false, muted = true, acquiring = false, generation = 0, muteVersion = 0
		var media = null, current = null, queue = Promise.resolve(), recorders = new Set()
		function wait(ms) { return new Promise(function (resolve) { setTimeout(resolve, ms) }) }
		function newStreamId() {
			var id = new Uint8Array(18); crypto.getRandomValues(id)
			return btoa(String.fromCharCode.apply(null, id)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "")
		}
		function closeStream(releaseMedia) {
			var item = current
			current = null
			if (item) {
				clearTimeout(item.rotate)
				if (item.rec.state !== "inactive") item.rec.stop()
				// An inactive recorder may still have its final dataavailable/onstop
				// events queued. Only onstop can confirm that all chunks were emitted.
			}
			if (releaseMedia) {
				generation++; acquiring = false
				stopTracks(media); media = null
			}
			return item ? item.done : Promise.resolve()
		}
		function upload(url, blob, attempt, finalizing) {
			if (failed) return Promise.resolve()
			var headers = { "X-CSRF-Token": d.csrf }
			if (blob) headers["Content-Type"] = "audio/webm"
			return fetch(url, {
				method: "POST", headers: headers,
				body: blob || "", credentials: "same-origin", keepalive: !blob || blob.size < 60000,
			}).then(function (response) {
				if (response.ok) return
				if (response.status >= 500 && attempt < 3) return wait(2000 * (attempt + 1)).then(function () { return upload(url, blob, attempt + 1, finalizing) })
				throw new Error("upload " + response.status)
			}).catch(function (err) {
				if (attempt < 3 && !(err && /upload/.test(err.message))) return wait(2000 * (attempt + 1)).then(function () { return upload(url, blob, attempt + 1, finalizing) })
				failed = true
				closeStream(true)
					say(finalizing ? "No se confirmó el cierre de la captura. Revisión requerida en administración." : "La subida de audio se interrumpió. Revisa la captura en administración.", "off")
			})
		}
		function recordStream() {
			if (stopped || failed || muted || current || !media) return
			var track = media.getAudioTracks()[0]
			if (!track) { failed = true; closeStream(true); say("No hay una pista de micrófono disponible.", "off"); return }
			var rec
			try { rec = new MediaRecorder(media, policy.recorderOptions(mime)) }
			catch (err) { failed = true; closeStream(true); say("No se pudo iniciar la captura de este micrófono.", "off"); return }
			var streamId = newStreamId(), start = Date.now()
			var info = policy.metadata(track.getSettings ? track.getSettings() : {}, rec, track.label, signal)
			var item = { rec: rec, rotate: null, resolve: null, done: null }
			item.done = new Promise(function (resolve) { item.resolve = resolve })
			recorders.add(item)
			var streamLanguage = options.language, streamShared = options.shared
			var uploads = policy.createStreamUploadSession({
				enqueue: function (task) { queue = queue.then(task); return queue },
				isFailed: function () { return failed },
				upload: function (seq, blob, end) {
					var params = new URLSearchParams({ stream: streamId, start: String(start), end: String(end), seq: String(seq),
						language: streamLanguage, shared: streamShared ? "1" : "0", metadata: JSON.stringify(info) })
					return upload("/m/" + encodeURIComponent(d.room) + "/audio?" + params.toString(), blob, 0, false)
				},
				finalize: function (manifest) {
					var params = new URLSearchParams({ stream: streamId, seq: String(manifest.seq), bytes: String(manifest.bytes), end: String(manifest.end) })
					return upload("/m/" + encodeURIComponent(d.room) + "/audio/finalize?" + params.toString(), null, 0, true)
				},
			})
			function settleRecorder() { recorders.delete(item); item.resolve() }
			rec.onstop = function () {
				clearTimeout(item.rotate)
				if (current === item) current = null
				uploads.finish(Date.now()) // Queued after the final dataavailable upload.
				settleRecorder()
			}
			rec.onerror = function () { failed = true; closeStream(true); say("La captura de audio se interrumpió.", "off"); settleRecorder() }
			rec.ondataavailable = function (ev) {
				if (failed || !ev.data || !ev.data.size) return
				// The event timestamp and metadata are frozen before any network retry.
				try { uploads.push(ev.data, Date.now()) } // Preserve the browser's bytes.
				catch (err) { failed = true; closeStream(true); say("La captura no pudo cerrarse de forma completa. Revisión requerida.", "off") }
			}
			current = item
			try { rec.start(policy.CHUNK_MS) }
			catch (err) { failed = true; settleRecorder(); closeStream(true); say("No se pudo iniciar la captura.", "off"); return }
			item.rotate = setTimeout(function () { closeStream(false).then(openStream) }, policy.ROTATE_MS)
			say("Capturando audio; procesamiento requiere aprobación")
		}
		function openStream() {
			if (stopped || failed || muted || current || acquiring) return
			if (media) { recordStream(); return }
			acquiring = true
			var version = ++generation
			say("Abriendo el micrófono elegido para capturar audio...")
			mediaDevices.getUserMedia(policy.constraints(options.deviceId, mediaDevices.getSupportedConstraints ? mediaDevices.getSupportedConstraints() : {})).then(function (stream) {
				if (version !== generation || stopped || muted || failed) { stopTracks(stream); return }
				acquiring = false; media = stream
				var track = stream.getAudioTracks()[0]
				if (track) {
					options.deviceLabel = track.label
					alignJitsi(track.label)
					track.addEventListener("ended", function () {
						if (media !== stream || stopped || muted) return
						closeStream(true)
						deviceStatus.textContent = "El micrófono dejó de estar disponible. Selecciona otro para continuar la captura."
						listDevices().then(function () {
							if (!stopped && !muted) capture.changeDevice(device.value, selectedLabel)
						}).catch(function () {})
					})
				}
				recordStream()
			}).catch(function () {
				if (version !== generation || stopped) return
				acquiring = false
				say("Captura detenida: revisa permisos y selecciona un micrófono disponible.", "off")
			})
		}
		function setMuted(value) {
			muted = value
			if (stopped || failed) return
			if (muted) {
				closeStream(true)
				say("Micrófono silenciado: la captura está pausada.", "muted")
			} else openStream()
		}
		var muteListener = function (event) { muteVersion++; setMuted(event.muted === true) }
		api.addEventListener("audioMuteStatusChanged", muteListener)
		var initialMuteVersion = muteVersion
		api.isAudioMuted().then(function (value) { if (muteVersion === initialMuteVersion) setMuted(value) })
			.catch(function () { say("No se pudo confirmar el estado del micrófono; silencia y activa el audio en Jitsi.", "off") })
		return {
			changeDevice: function (id, label) {
				options.deviceId = id; options.deviceLabel = label
				return closeStream(true).then(function () { alignJitsi(label); openStream() })
			},
			stop: function () {
				stopped = true
				if (api.removeEventListener) api.removeEventListener("audioMuteStatusChanged", muteListener)
				closeStream(true)
				// Include recorders already closing because of mute, rotation or a
				// device change, whose final browser event may not have run yet.
				return Promise.all(Array.from(recorders, function (item) { return item.done }))
					.then(function () { return queue }).then(function () { badge.remove(); return !failed })
			},
		}
	}
})()
