// Browser-side audio capture for the in-process auto-generator.
//
// The teacher grants microphone access from the Audio card on /status.
// We start a MediaRecorder, stop it every poll_interval_seconds, POST
// the resulting Opus/WebM blob to /audio/segment, then immediately
// start a new recording so transcription gaps are bounded by stop+start
// latency (10–50 ms) rather than by the request round-trip.
//
// Mute pauses the recorder (the resulting blob skips paused intervals
// natively). The device picker lists every audio input enumerateDevices
// returns; switching restarts capture with the chosen deviceId, saved
// to localStorage so the choice survives reloads.

(function () {
	const card = document.getElementById("audio-card");
	if (!card) return;

	const pre = document.getElementById("audio-pre");
	const live = document.getElementById("audio-live");
	const grantBtn = document.getElementById("audio-grant-btn");
	const muteBtn = document.getElementById("audio-mute-btn");
	const triggerBtn = document.getElementById("audio-trigger-btn");
	const deviceSel = document.getElementById("audio-device");
	const statusEl = document.getElementById("audio-status");

	const pollInterval = Math.max(parseInt(card.dataset.pollInterval, 10) || 300, 30);
	const deviceKey = "ptrack.audio.deviceId";

	let stream = null;
	let recorder = null;
	let intervalHandle = null;
	let muted = false;
	let inFlight = false;
	let currentBlobs = [];

	const supportedMime = pickSupportedMime();

	function pickSupportedMime() {
		const candidates = [
			"audio/webm;codecs=opus",
			"audio/webm",
			"audio/ogg;codecs=opus",
			"audio/mp4",
		];
		for (const m of candidates) {
			if (typeof MediaRecorder !== "undefined" && MediaRecorder.isTypeSupported && MediaRecorder.isTypeSupported(m)) {
				return m;
			}
		}
		return "";
	}

	function setStatus(text) {
		if (statusEl) statusEl.textContent = text;
	}

	function showLive() {
		pre.hidden = true;
		live.hidden = false;
	}

	async function listDevices() {
		try {
			const devices = await navigator.mediaDevices.enumerateDevices();
			const inputs = devices.filter((d) => d.kind === "audioinput");
			deviceSel.innerHTML = "";
			const saved = localStorage.getItem(deviceKey) || "";
			inputs.forEach((d) => {
				const o = document.createElement("option");
				o.value = d.deviceId;
				o.textContent = d.label || "input (" + d.deviceId.slice(0, 6) + ")";
				if (d.deviceId === saved) o.selected = true;
				deviceSel.appendChild(o);
			});
		} catch (e) {
			console.warn("enumerateDevices failed", e);
		}
	}

	async function startStream(deviceId) {
		const constraints = { audio: deviceId ? { deviceId: { exact: deviceId } } : true };
		stream = await navigator.mediaDevices.getUserMedia(constraints);
	}

	function startRecorder() {
		if (!stream) return;
		const opts = supportedMime ? { mimeType: supportedMime } : undefined;
		recorder = new MediaRecorder(stream, opts);
		currentBlobs = [];
		recorder.ondataavailable = (e) => {
			if (e.data && e.data.size > 0) currentBlobs.push(e.data);
		};
		recorder.onstop = onSegmentStop;
		recorder.start();
	}

	function onSegmentStop() {
		const blob = new Blob(currentBlobs, { type: supportedMime || "audio/webm" });
		startRecorder();
		if (blob.size === 0) return;
		postSegment(blob);
	}

	async function postSegment(blob) {
		if (inFlight) return;
		inFlight = true;
		setStatus("uploading " + Math.round(blob.size / 1024) + " kB…");
		try {
			const resp = await fetch("/audio/segment", {
				method: "POST",
				headers: { "Content-Type": blob.type || "audio/webm" },
				body: blob,
			});
			const body = await resp.json().catch(() => ({}));
			renderResult(body);
		} catch (err) {
			setStatus("error: " + err);
		} finally {
			inFlight = false;
		}
	}

	function renderResult(body) {
		const now = new Date().toLocaleTimeString();
		switch (body.status) {
			case "generated":
				setStatus("[" + now + "] generated " + (body.questions || 0) + " question(s)");
				break;
			case "skipped":
				if (body.reason === "silence_or_too_short") {
					setStatus("[" + now + "] silent interval");
				} else {
					setStatus("[" + now + "] holding (" + (body.words || 0) + "/" + (body.needed || 0) + " words)");
				}
				break;
			case "failed":
				setStatus("[" + now + "] failed: " + (body.reason || "unknown"));
				break;
			default:
				setStatus("[" + now + "] " + JSON.stringify(body));
		}
	}

	function startInterval() {
		clearInterval(intervalHandle);
		intervalHandle = setInterval(() => {
			if (recorder && recorder.state === "recording") {
				recorder.stop();
			}
		}, pollInterval * 1000);
	}

	function setMuted(next) {
		muted = next;
		if (!recorder) return;
		if (muted && recorder.state === "recording") {
			recorder.pause();
		} else if (!muted && recorder.state === "paused") {
			recorder.resume();
		}
		muteBtn.textContent = muted ? muteBtn.dataset.unmuteLabel || "Unmute" : muteBtn.dataset.muteLabel || "Mute";
	}

	async function init() {
		if (!supportedMime) {
			setStatus("MediaRecorder not supported in this browser");
			grantBtn.disabled = true;
			return;
		}
		muteBtn.dataset.muteLabel = muteBtn.textContent;
		muteBtn.dataset.unmuteLabel = muteBtn.textContent === "Mute" ? "Unmute" : muteBtn.textContent;

		grantBtn.addEventListener("click", async () => {
			grantBtn.disabled = true;
			try {
				const saved = localStorage.getItem(deviceKey) || "";
				await startStream(saved);
				await listDevices();
				startRecorder();
				startInterval();
				showLive();
				setStatus("recording…");
			} catch (e) {
				grantBtn.disabled = false;
				setStatus("permission denied: " + e);
			}
		});

		muteBtn.addEventListener("click", () => setMuted(!muted));

		triggerBtn.addEventListener("click", () => {
			if (recorder && recorder.state !== "inactive") recorder.stop();
		});

		deviceSel.addEventListener("change", async () => {
			localStorage.setItem(deviceKey, deviceSel.value);
			try {
				if (recorder && recorder.state !== "inactive") recorder.stop();
				if (stream) stream.getTracks().forEach((t) => t.stop());
				await startStream(deviceSel.value);
				startRecorder();
			} catch (e) {
				setStatus("device change failed: " + e);
			}
		});
	}

	init();
})();
