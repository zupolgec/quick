package main

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type lang string

const (
	langEN lang = "en"
	langIT lang = "it"
)

// pickLang selects the UI language for a server-rendered page: a ?lang override
// wins, then the browser's Accept-Language, falling back to English. Only en/it
// are supported for now.
func pickLang(r *http.Request) lang {
	if q := r.URL.Query().Get("lang"); q != "" {
		if l, ok := matchLang(q); ok {
			return l
		}
	}
	for _, tag := range acceptLanguages(r.Header.Get("Accept-Language")) {
		if l, ok := matchLang(tag); ok {
			return l
		}
	}
	return langEN
}

func matchLang(tag string) (lang, bool) {
	tag = strings.ToLower(strings.TrimSpace(tag))
	switch {
	case tag == "it" || strings.HasPrefix(tag, "it-"):
		return langIT, true
	case tag == "en" || strings.HasPrefix(tag, "en-"):
		return langEN, true
	}
	return "", false
}

// acceptLanguages returns the header's language tags ordered by descending
// q-value (so the browser's preferred language comes first).
func acceptLanguages(h string) []string {
	if h == "" {
		return nil
	}
	type weighted struct {
		tag string
		q   float64
	}
	var ws []weighted
	for part := range strings.SplitSeq(h, ",") {
		tag := strings.TrimSpace(part)
		if tag == "" {
			continue
		}
		q := 1.0
		if t, params, found := strings.Cut(tag, ";"); found {
			tag = strings.TrimSpace(t)
			if _, qv, ok := strings.Cut(params, "q="); ok {
				q, _ = strconv.ParseFloat(strings.TrimSpace(qv), 64)
			}
		}
		ws = append(ws, weighted{tag, q})
	}
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].q > ws[j].q })
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.tag
	}
	return out
}

// uiText is the user-facing copy for the server-rendered pages. Fragments that
// sit next to inline <code>/<b> markup in the templates are split so the markup
// stays in the template and only the prose is translated.
type uiText struct {
	NavDashboard, Copy string

	LandingTitle, LandingHeadline, LandingTagline, LandingGetStarted string
	LandingInstall, LandingLogin, LandingDeploy                      string

	GuideTitle, GuideUpdate, GuideVisibility, GuideStatus, GuideRollback string

	SSOTitle, SSOHeading, SSOIntro, SSOButton string

	DashTitle, DashYourSites, DashAllSites  string
	DashEmptyMine, DashEmptyAll, DashLocked string
	DashHelpPublish, DashHelpInstall        string
	BadgePublic, BadgeCode, BadgeSSO        string
	DashManage                              string

	SiteBack, SiteNoFiles, SiteTokens, SiteTokensHint, SiteTokensEmpty  string
	TokenCreated, TokenStoreAs, TokenNotShown                           string
	TokenScope, TokenExpires, TokenNever, TokenLastUsed, TokenNeverUsed string
	TokenRevoke, TokenCreate, TokenName, TokenExpiry, TokenDays         string
	TokenNoExpiry                                                       string

	CodeTitle, CodeHeading, CodeIntro string
	CodeLabel, CodeButton, CodeError  string
}

var uiTexts = map[lang]uiText{
	langEN: {
		NavDashboard: "Dashboard",
		Copy:         "Copy",

		LandingTitle:      "quick — static hosting",
		LandingHeadline:   "Publish a folder, get a URL.",
		LandingTagline:    "Internal static hosting, behind your company sign-in.",
		LandingGetStarted: "Get started",
		LandingInstall:    "Install the CLI",
		LandingLogin:      "Sign in",
		LandingDeploy:     "Publish a folder",

		GuideTitle:      "How it works",
		GuideUpdate:     "Re-run it to update: every deploy replaces the site's files.",
		GuideVisibility: "Choose who can open the site: company SSO, an access code, or public.",
		GuideStatus:     "Check the site's URL, visibility and last deploy.",
		GuideRollback:   "Restore the previous version when something goes wrong.",

		SSOTitle:   "quick — access",
		SSOHeading: "Sign-in required",
		SSOIntro:   "Sign in with your company account to continue to",
		SSOButton:  "Sign in with Google",

		DashTitle:       "quick — your sites",
		DashYourSites:   "Your sites",
		DashAllSites:    "All sites",
		DashEmptyMine:   "You haven't published any sites yet. Use",
		DashEmptyAll:    "No sites published.",
		DashLocked:      "locked",
		DashHelpPublish: "Publish a folder:",
		DashHelpInstall: "Install the CLI with",
		BadgePublic:     "public",
		DashManage:      "Manage",

		SiteBack:        "Back to dashboard",
		SiteNoFiles:     "No files",
		SiteTokens:      "Deploy tokens",
		SiteTokensHint:  "Tokens can only deploy this site. They cannot delete it, roll it back, change its visibility or create other tokens.",
		SiteTokensEmpty: "No deploy tokens yet.",
		TokenCreated:    "Deploy token created.",
		TokenStoreAs:    "Store it as",
		TokenNotShown:   "It will not be shown again.",
		TokenScope:      "scope",
		TokenExpires:    "expires",
		TokenNever:      "never",
		TokenLastUsed:   "last used",
		TokenNeverUsed:  "never used",
		TokenRevoke:     "Revoke",
		TokenCreate:     "Create token",
		TokenName:       "Name",
		TokenExpiry:     "Expires",
		TokenDays:       "days",
		TokenNoExpiry:   "Never",
		BadgeCode:       "code",
		BadgeSSO:        "SSO",

		CodeTitle:   "Protected access",
		CodeHeading: "Protected site",
		CodeIntro:   "Enter the access code for",
		CodeLabel:   "Code",
		CodeButton:  "Enter",
		CodeError:   "Wrong code, try again.",
	},
	langIT: {
		NavDashboard: "Dashboard",
		Copy:         "Copia",

		LandingTitle:      "quick · hosting statico",
		LandingHeadline:   "Pubblica una cartella, ottieni un URL.",
		LandingTagline:    "Hosting statico interno, dietro l'accesso aziendale.",
		LandingGetStarted: "Come iniziare",
		LandingInstall:    "Installa la CLI",
		LandingLogin:      "Accedi",
		LandingDeploy:     "Pubblica una cartella",

		GuideTitle:      "Come funziona",
		GuideUpdate:     "Ripubblica per aggiornare: ogni deploy sostituisce i file del sito.",
		GuideVisibility: "Scegli chi può aprire il sito: SSO aziendale, codice di accesso o pubblico.",
		GuideStatus:     "Controlla URL, visibilità e ultimo deploy del sito.",
		GuideRollback:   "Torna alla versione precedente quando qualcosa va storto.",

		SSOTitle:   "quick · accesso",
		SSOHeading: "Accesso richiesto",
		SSOIntro:   "Accedi con l'account aziendale per continuare su",
		SSOButton:  "Accedi con Google",

		DashTitle:       "quick · i tuoi siti",
		DashYourSites:   "I tuoi siti",
		DashAllSites:    "Tutti i siti",
		DashEmptyMine:   "Non hai ancora pubblicato nessun sito. Usa",
		DashEmptyAll:    "Nessun sito pubblicato.",
		DashLocked:      "bloccato",
		DashHelpPublish: "Pubblica una cartella:",
		DashHelpInstall: "Installa la CLI con",
		BadgePublic:     "pubblico",
		DashManage:      "Gestisci",

		SiteBack:        "Torna alla dashboard",
		SiteNoFiles:     "Nessun file",
		SiteTokens:      "Token di deploy",
		SiteTokensHint:  "I token possono solo pubblicare questo sito: non possono eliminarlo, riportarlo alla versione precedente, cambiarne la visibilità o creare altri token.",
		SiteTokensEmpty: "Nessun token di deploy.",
		TokenCreated:    "Token di deploy creato.",
		TokenStoreAs:    "Salvalo come",
		TokenNotShown:   "Non verrà più mostrato.",
		TokenScope:      "ambito",
		TokenExpires:    "scade",
		TokenNever:      "mai",
		TokenLastUsed:   "ultimo uso",
		TokenNeverUsed:  "mai usato",
		TokenRevoke:     "Revoca",
		TokenCreate:     "Crea token",
		TokenName:       "Nome",
		TokenExpiry:     "Scadenza",
		TokenDays:       "giorni",
		TokenNoExpiry:   "Mai",
		BadgeCode:       "codice",
		BadgeSSO:        "SSO",

		CodeTitle:   "Accesso protetto",
		CodeHeading: "Sito protetto",
		CodeIntro:   "Inserisci il codice di accesso per",
		CodeLabel:   "Codice",
		CodeButton:  "Entra",
		CodeError:   "Codice errato, riprova.",
	},
}

func textsFor(l lang) uiText {
	if t, ok := uiTexts[l]; ok {
		return t
	}
	return uiTexts[langEN]
}

// badgeLabel maps a site's access mode to its localized dashboard badge.
func badgeLabel(access string, t uiText) string {
	switch access {
	case "public":
		return t.BadgePublic
	case "code":
		return t.BadgeCode
	default:
		return t.BadgeSSO
	}
}
