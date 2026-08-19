// Client-side file download for the canonical export (ticket 17.6). The
// artifact viewer sandboxes downloads, but this app runs on the normal web
// origin, so a Blob + object-URL anchor click is the ordinary path.

/** Trigger a browser download of `text` as `filename`. */
export function downloadText(filename: string, text: string, mime = "application/json"): void {
  const blob = new Blob([text], { type: mime });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Revoke on the next tick so the click has consumed the URL.
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

/** A safe file base name from a definition name (fallback `workflow`). */
export function definitionFilename(name: unknown): string {
  const base = typeof name === "string" && name.trim() ? name.trim() : "workflow";
  const safe = base.replace(/[^a-zA-Z0-9._-]+/g, "-").replace(/^-+|-+$/g, "") || "workflow";
  return `${safe}.json`;
}
