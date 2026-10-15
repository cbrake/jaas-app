// Join and waiting pages: host-password section toggle and, on the waiting
// page, a periodic re-submit while the host has not started the meeting.
;(function () {
	var section = document.getElementById("host-section")
	var toggle = document.getElementById("host-toggle")
	if (toggle && section) {
		toggle.addEventListener("click", function () {
			section.classList.toggle("hidden")
			if (!section.classList.contains("hidden")) {
				var input = section.querySelector("input[type=password]")
				if (input) input.focus()
			}
		})
	}
	var rejoin = document.getElementById("rejoin")
	if (rejoin) {
		setInterval(function () {
			if (!section || section.classList.contains("hidden")) rejoin.submit()
		}, 5000)
	}
})()
