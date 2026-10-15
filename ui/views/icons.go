// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package views

import (
	"context"
	"io"
	"strings"

	"github.com/a-h/templ"
)

// Icons are inline SVG (no icon font: the agent's CSP allows no font
// downloads). Outline icons use the current text colour as stroke; status
// icons are filled.

var outlineIcons = map[string]string{
	"apps":     `<path d="M12 3 3 7.5 12 12l9-4.5z"/><path d="m3 12 9 4.5 9-4.5"/><path d="m3 16.5 9 4.5 9-4.5"/>`,
	"settings": `<path d="M19.41 10.30 L21.86 10.36 L21.86 13.64 L19.41 13.70 L18.44 16.04 L20.14 17.81 L17.81 20.14 L16.04 18.44 L13.70 19.41 L13.64 21.86 L10.36 21.86 L10.30 19.41 L7.96 18.44 L6.19 20.14 L3.86 17.81 L5.56 16.04 L4.59 13.70 L2.14 13.64 L2.14 10.36 L4.59 10.30 L5.56 7.96 L3.86 6.19 L6.19 3.86 L7.96 5.56 L10.30 4.59 L10.36 2.14 L13.64 2.14 L13.70 4.59 L16.04 5.56 L17.81 3.86 L20.14 6.19 L18.44 7.96z"/><circle cx="12" cy="12" r="3"/>`,
	"user":     `<circle cx="12" cy="8" r="4"/><path d="M4 21c0-4.4 3.6-7 8-7s8 2.6 8 7"/>`,
	"book":     `<path d="M2 5h6a4 4 0 0 1 4 4v11a3 3 0 0 0-3-3H2z"/><path d="M22 5h-6a4 4 0 0 0-4 4v11a3 3 0 0 1 3-3h7z"/>`,
	"logout":   `<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="m16 17 5-5-5-5"/><path d="M21 12H9"/>`,
	"plus":     `<path d="M12 5v14M5 12h14"/>`,
	"sync":     `<path d="M20 12a8 8 0 0 1-13.7 5.6"/><path d="M4 12a8 8 0 0 1 13.7-5.6"/><path d="M17.7 2.5v4h-4"/><path d="M6.3 21.5v-4h4"/>`,
	"refresh":  `<path d="M20 11a8 8 0 1 0-2.3 6.3"/><path d="M20 4v7h-7"/>`,
	"trash":    `<path d="M4 7h16"/><path d="M9 7V4h6v3"/><path d="m6 7 1 13h10l1-13"/><path d="M10 11v6M14 11v6"/>`,
	"search":   `<circle cx="11" cy="11" r="7"/><path d="m21 21-5-5"/>`,
	"tiles":    `<rect x="3" y="3" width="7.5" height="7.5" rx="1"/><rect x="13.5" y="3" width="7.5" height="7.5" rx="1"/><rect x="3" y="13.5" width="7.5" height="7.5" rx="1"/><rect x="13.5" y="13.5" width="7.5" height="7.5" rx="1"/>`,
	"list":     `<path d="M9 6h12M9 12h12M9 18h12"/><path d="M4 6h.01M4 12h.01M4 18h.01"/>`,
	"pie":      `<path d="M21 12A9 9 0 1 1 12 3v9z"/><path d="M15 3.5A9 9 0 0 1 20.5 9H15z"/>`,
	"filter":   `<path d="M3 4h18l-7 8.5V19l-4 2v-8.5z"/>`,
	"star":     `<path d="M12.00 3.00 L14.41 9.28 L21.13 9.63 L15.90 13.87 L17.64 20.37 L12.00 16.70 L6.36 20.37 L8.10 13.87 L2.87 9.63 L9.59 9.28z"/>`,
	"external": `<path d="M14 4h6v6"/><path d="M20 4 11 13"/><path d="M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/>`,
	"branch":   `<circle cx="6" cy="5" r="2"/><circle cx="6" cy="19" r="2"/><circle cx="18" cy="7" r="2"/><path d="M6 7v10"/><path d="M18 9c0 5-6 4-11.5 8.5"/>`,
	"history":  `<path d="M3 12a9 9 0 1 0 3-6.7"/><path d="M3 4v5h5"/><path d="M12 7v5l3 2"/>`,
	"info":     `<circle cx="12" cy="12" r="9"/><path d="M12 11v6"/><path d="M12 7.5h.01"/>`,
	"diff":     `<path d="M14 3H6a1 1 0 0 0-1 1v16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8z"/><path d="M14 3v5h5"/><path d="M9 11h6M12 8.5v5M9 17h6"/>`,
	"dots":     `<circle cx="12" cy="5" r="1.7" fill="currentColor" stroke="none"/><circle cx="12" cy="12" r="1.7" fill="currentColor" stroke="none"/><circle cx="12" cy="19" r="1.7" fill="currentColor" stroke="none"/>`,
	"close":    `<path d="M6 6l12 12M18 6 6 18"/>`,
	"down":     `<path d="m6 9 6 6 6-6"/>`,
	"right":    `<path d="m9 6 6 6-6 6"/>`,
	"collapse": `<path d="m11 7-5 5 5 5"/><path d="m18 7-5 5 5 5"/>`,
	"terminal": `<rect x="3" y="4" width="18" height="16" rx="2"/><path d="m7 9 3 3-3 3"/><path d="M12 15h5"/>`,
	"server":   `<rect x="3" y="3" width="18" height="7" rx="1.5"/><rect x="3" y="14" width="18" height="7" rx="1.5"/><path d="M7 6.5h.01M7 17.5h.01"/>`,
	"key":      `<circle cx="8" cy="15" r="4"/><path d="m11 12 9-9"/><path d="m17 6 3 3"/><path d="m14 9 2 2"/>`,
	"sun":      `<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>`,
	"git":      `<path d="M3.5 12 12 3.5l8.5 8.5-8.5 8.5z"/><circle cx="12" cy="8.5" r="1.3"/><circle cx="12" cy="15.5" r="1.3"/><circle cx="15.5" cy="12" r="1.3"/><path d="M12 9.8v4.4M13.2 11.2l1.2.4"/>`,
	"folder":   `<path d="M3 6a1 1 0 0 1 1-1h5l2 2h9a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1z"/>`,
	"copy":     `<rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V5a1 1 0 0 0-1-1H5a1 1 0 0 0-1 1v10a1 1 0 0 0 1 1h3"/>`,
	"download": `<path d="M12 4v11"/><path d="m7 10 5 5 5-5"/><path d="M5 20h14"/>`,
	"edit":     `<path d="M4 20h4L19 9l-4-4L4 16z"/><path d="m13.5 6.5 4 4"/>`,
	"check":    `<path d="m5 12.5 4.5 4.5L19 7.5"/>`,
	"warning":  `<path d="M12 3 2 20h20z"/><path d="M12 10v4.5"/><path d="M12 17.5h.01"/>`,
	"stop":     `<rect x="6" y="6" width="12" height="12" rx="1.5"/>`,
	"play":     `<path d="M7 4.5v15L19.5 12z"/>`,
	"pause":    `<path d="M8 5v14M16 5v14"/>`,
	"cube":     `<path d="M12 2.5 3.5 7v10l8.5 4.5 8.5-4.5V7z"/><path d="m3.5 7 8.5 4.5L20.5 7"/><path d="M12 11.5v10"/>`,
	"job":      `<path d="M12 2.5 20.5 7.25v9.5L12 21.5 3.5 16.75v-9.5z"/><path d="M9 15.5v-7l6 7v-7"/>`,
	"group":    `<rect x="3" y="3" width="8" height="8" rx="1.5"/><rect x="13" y="3" width="8" height="8" rx="1.5"/><rect x="3" y="13" width="8" height="8" rx="1.5"/><rect x="13" y="13" width="8" height="8" rx="1.5"/>`,
	"task":     `<rect x="6" y="6" width="12" height="12" rx="1.5"/><path d="M9 2.5v3.5M15 2.5v3.5M9 18v3.5M15 18v3.5M2.5 9h3.5M2.5 15h3.5M18 9h3.5M18 15h3.5"/>`,
	"deploy":   `<path d="M4 12h15"/><path d="m13 6 6 6-6 6"/><path d="M4 6v12"/>`,
	"service":  `<circle cx="12" cy="5" r="2.5"/><circle cx="5" cy="19" r="2.5"/><circle cx="19" cy="19" r="2.5"/><path d="M12 7.5v4M12 11.5 6.5 17M12 11.5l5.5 5.5"/>`,
	"database": `<ellipse cx="12" cy="5.5" rx="8" ry="3"/><path d="M4 5.5v13c0 1.7 3.6 3 8 3s8-1.3 8-3v-13"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/>`,
	"shield":   `<path d="M12 2.5 4 5.5v6c0 5 3.4 8.6 8 10 4.6-1.4 8-5 8-10v-6z"/><path d="m8.5 12 2.5 2.5 4.5-5"/>`,
	"lock":     `<rect x="4.5" y="10.5" width="15" height="10.5" rx="1.5"/><path d="M8 10.5V7a4 4 0 0 1 8 0v3.5"/>`,
	"link":     `<path d="M10 14a4.5 4.5 0 0 0 6.4 0l3.2-3.2a4.5 4.5 0 0 0-6.4-6.4L11.6 6"/><path d="M14 10a4.5 4.5 0 0 0-6.4 0l-3.2 3.2a4.5 4.5 0 0 0 6.4 6.4l1.6-1.6"/>`,
	"heart":    `<path d="M12 20.5 10.8 19.4C6.2 15.3 3 12.5 3 8.9 3 6.1 5.2 4 8 4c1.6 0 3 .7 4 1.9C13 4.7 14.4 4 16 4c2.8 0 5 2.1 5 4.9 0 3.6-3.2 6.4-7.8 10.5z"/>`,
}

var filledIcons = map[string]string{
	"Healthy":     `<path d="M12 21.2 10.6 20C5.4 15.4 2 12.4 2 8.6 2 5.6 4.4 3.2 7.4 3.2c1.7 0 3.4.8 4.6 2.1 1.2-1.3 2.9-2.1 4.6-2.1 3 0 5.4 2.4 5.4 5.4 0 3.8-3.4 6.8-8.6 11.4z"/>`,
	"Degraded":    `<path d="M12 21.2 10.6 20C5.4 15.4 2 12.4 2 8.6 2 5.6 4.4 3.2 7.4 3.2c1.7 0 3.4.8 4.6 2.1 1.2-1.3 2.9-2.1 4.6-2.1 3 0 5.4 2.4 5.4 5.4 0 3.8-3.4 6.8-8.6 11.4z"/><path class="crack" d="M12.6 5.2 10.4 10l3.1 2.4-2.2 5" fill="none" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/>`,
	"Progressing": `<path d="M12 2.5a9.5 9.5 0 1 0 9.5 9.5" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round"/>`,
	"Suspended":   `<path fill-rule="evenodd" d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20zM9 8h2v8H9zm4 0h2v8h-2z"/>`,
	"Missing":     `<path fill-rule="evenodd" d="M12 2a8 8 0 0 0-8 8v12l2.7-2 2.6 2L12 20l2.7 2 2.6-2L20 22V10a8 8 0 0 0-8-8zM9 8.5a1.5 1.5 0 1 0 0 3 1.5 1.5 0 0 0 0-3zm6 0a1.5 1.5 0 1 0 0 3 1.5 1.5 0 0 0 0-3z"/>`,
	"Unknown":     `<path fill-rule="evenodd" d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20zm0 14.2a1.2 1.2 0 1 0 0 2.4 1.2 1.2 0 0 0 0-2.4zM12 5.8c-2.2 0-3.8 1.4-3.9 3.4h2.2c.1-.9.8-1.5 1.7-1.5 1 0 1.7.6 1.7 1.4 0 .7-.3 1.1-1.2 1.7-1 .6-1.6 1.4-1.5 2.6v.6h2.1v-.5c0-.8.3-1.2 1.3-1.8 1-.6 1.6-1.4 1.6-2.6 0-1.9-1.7-3.3-4-3.3z"/>`,
	"Synced":      `<path fill-rule="evenodd" d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20zm4.3 6.3-5.6 5.6-2.9-2.9-1.4 1.4 4.3 4.3 7-7z"/>`,
	"OutOfSync":   `<path fill-rule="evenodd" d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20zm0 4.6-5.2 5.2 1.4 1.4 2.8-2.8V17h2v-6.6l2.8 2.8 1.4-1.4z"/>`,
	"Failed":      `<path fill-rule="evenodd" d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20zM8.5 7.1 7.1 8.5 10.6 12l-3.5 3.5 1.4 1.4 3.5-3.5 3.5 3.5 1.4-1.4-3.5-3.5 3.5-3.5-1.4-1.4-3.5 3.5z"/>`,
	"Pruned":      `<path fill-rule="evenodd" d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20zM7 11h10v2H7z"/>`,
	"Warning":     `<path fill-rule="evenodd" d="M12 2.5 1.5 21h21zm-1 7h2v5.5h-2zm0 7h2v2h-2z"/>`,
}

// statusAliases maps status vocabulary onto the filled icon set.
var statusAliases = map[string]string{
	"Succeeded": "Synced", "Successful": "Synced", "Running": "Progressing", "Terminating": "Progressing",
	"Error": "Failed", "SyncFailed": "Failed", "PruneSkipped": "Warning", "": "Unknown",
}

// Icon renders an outline icon.
func Icon(name string, class ...string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		body, ok := outlineIcons[name]
		if !ok {
			body = outlineIcons["cube"]
		}
		_, err := io.WriteString(w, `<svg class="icon`+classes(class)+`" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">`+body+`</svg>`)
		return err
	})
}

// StatusIcon renders a filled health, sync or operation status icon in its
// status colour; the title is the status name.
func StatusIcon(status string, class ...string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		name := status
		if a, ok := statusAliases[name]; ok {
			name = a
		}
		body, ok := filledIcons[name]
		if !ok {
			name, body = "Unknown", filledIcons["Unknown"]
		}
		extra := classes(class)
		if name == "Progressing" {
			extra += " icon--spin"
		}
		label := status
		if label == "" {
			label = "Unknown"
		}
		_, err := io.WriteString(w, `<svg class="icon st-`+templ.EscapeString(statusClass(status))+extra+`" viewBox="0 0 24 24" fill="currentColor" role="img" aria-label="`+templ.EscapeString(label)+`"><title>`+templ.EscapeString(label)+`</title>`+body+`</svg>`)
		return err
	})
}

func classes(c []string) string {
	if len(c) == 0 {
		return ""
	}
	return " " + templ.EscapeString(strings.Join(c, " "))
}

func statusClass(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}
