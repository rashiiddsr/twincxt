//go:build windows

package main

import (
	_ "embed"
	"strings"
)

// serviceScriptTemplate (ui/service.js) runs in every WhatsApp / Telegram
// document before page code:
//   - notifications go to the native toast bridge (no Edge popups);
//   - the colour at the top of the page is reported for the title strip;
//   - WhatsApp's call window opens only from "Move to new window".
//
//go:embed ui/service.js
var serviceScriptTemplate string

func serviceScript(svc string) string {
	return strings.NewReplacer("__SVC__", svc, "__POPUP__", "false").Replace(serviceScriptTemplate)
}

// popupScript is the same minus the title-colour sampling, which call windows
// do not need (it would only add work while video is playing).
func popupScript(svc string) string {
	return strings.NewReplacer("__SVC__", svc, "__POPUP__", "true").Replace(serviceScriptTemplate)
}
