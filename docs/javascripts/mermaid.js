// Initialize Mermaid diagrams.
//
// Keep this tiny: we don't enable MkDocs Material instant navigation by default,
// so DOMContentLoaded is sufficient for now.
window.addEventListener("DOMContentLoaded", () => {
	if (typeof mermaid === "undefined") {
		return
	}

	mermaid.initialize({ startOnLoad: true })
})
