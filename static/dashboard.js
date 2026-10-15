// Admin dashboard helpers. Everything here is progressive enhancement: the
// forms work without JavaScript.
;(function () {
	// Copy meeting links to the clipboard.
	document.querySelectorAll("[data-copy]").forEach(function (btn) {
		btn.addEventListener("click", function () {
			navigator.clipboard.writeText(btn.dataset.copy).then(function () {
				var old = btn.textContent
				btn.textContent = "Copied"
				setTimeout(function () {
					btn.textContent = old
				}, 1500)
			})
		})
	})

	// Confirm destructive actions.
	document.querySelectorAll("form[data-confirm]").forEach(function (form) {
		form.addEventListener("submit", function (e) {
			if (!confirm(form.dataset.confirm)) e.preventDefault()
		})
	})

	// Submit the transcription toggle on change.
	document.querySelectorAll("input[data-autosubmit]").forEach(function (input) {
		input.addEventListener("change", function () {
			input.form.submit()
		})
	})

	// Remember the host display name used to start meetings.
	var KEY = "jaas_host_name"
	var saved = ""
	try {
		saved = localStorage.getItem(KEY) || ""
	} catch (e) {}
	document.querySelectorAll("input.start-name").forEach(function (input) {
		if (saved) input.value = saved
		input.addEventListener("input", function () {
			try {
				localStorage.setItem(KEY, input.value)
			} catch (e) {}
		})
	})

	// Show/hide password fields.
	document.querySelectorAll("[data-reveal]").forEach(function (btn) {
		btn.addEventListener("click", function () {
			var input = document.getElementById(btn.dataset.reveal)
			input.type = input.type === "password" ? "text" : "password"
			btn.textContent = input.type === "password" ? "Show" : "Hide"
		})
	})
})()
