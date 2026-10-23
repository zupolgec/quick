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

	DashTitle, DashYourSites, DashAllSites      string
	DashEmptyMine, DashEmptyAll, DashLocked     string
	DashHelpPublish, DashHelpDocs               string
	DashSearch, DashNoMatch, NavDocs, GuideMore string
	BadgePublic, BadgeCode, BadgeSSO            string
	BadgePeople, BadgeOnlyMe                    string
	DashManage                                  string

	AccessTitle, AccessOwner, AccessCurrent, AccessOwnerOnly, AccessSave string
	AccessCodeLabel, AccessCodeKeep                                      string
	LevelTeam, LevelTeamHint, LevelOnlyMe, LevelOnlyMeHint               string
	LevelPeople, LevelPeopleHint, LevelCode, LevelCodeHint               string
	LevelPublic, LevelPublicHint                                         string
	PeopleTitle, PeopleOnlyOwner, PeopleRemove, PeopleAdd, PeopleEmail   string
	PeopleEmailHint, PeopleEmailUser                                     string
	AccessErrNotOwnerA, AccessErrNotOwnerB, AccessErrNoOwner             string
	AccessErrBadEmail, AccessErrMissingEmail, AccessErrDomain            string
	AccessErrTooMany, AccessErrOwnerAlways, AccessErrCode                string
	AccessErrGeneric                                                     string

	RestrictedTitle, RestrictedHeading, RestrictedBy, RestrictedCanOpen string
	RestrictedSignedIn, RestrictedAsk, RestrictedSwitch                 string

	SizeTitle, SizeIntro, SizeAsk, SizeLabel, SizeHint, SizeSave string
	SizeInvalid, SizeTooHigh, SizeAskSys                         string
	AccessAdmins                                                 string

	SiteBack, SiteNoFiles, SiteTokens, SiteTokensHint, SiteTokensEmpty  string
	SiteTitle                                                           string
	TokenCreated, TokenStoreAs, TokenNotShown                           string
	TokenScope, TokenExpires, TokenNever, TokenLastUsed, TokenNeverUsed string
	TokenRevoke, TokenCreate, TokenName, TokenExpiry, TokenDays         string
	TokenNoExpiry                                                       string

	CodeTitle, CodeHeading, CodeIntro string
	CodeLabel, CodeButton, CodeError  string

	NotFoundTitle, NotFoundSiteHeading, NotFoundSiteIntro string
	NotFoundPageHeading, NotFoundPageIntro                string
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
		GuideVisibility: "Choose who can open the site: company SSO, chosen people, an access code, or public.",
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
		DashHelpDocs:    "Install the CLI and read the guides at",
		DashSearch:      "Search by name",
		DashNoMatch:     "No site matches.",
		NavDocs:         "Docs",
		GuideMore:       "All commands and guides",
		BadgePublic:     "public",
		DashManage:      "Manage",

		SiteBack:        "Back to dashboard",
		SiteTitle:       "Manage site",
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
		BadgePeople:     "chosen people",
		BadgeOnlyMe:     "only me",

		AccessTitle:     "Who can see this site",
		AccessOwner:     "Owner:",
		AccessCurrent:   "Right now:",
		AccessOwnerOnly: "Only the owner can change it from here.",
		AccessAdmins:    "Administrators (%s) can see and manage every site, including those limited to chosen people.",
		SizeTitle:       "Size limit",
		SizeIntro:       "Every version of this site can take up to",
		SizeAsk:         "To publish something bigger, ask an administrator: %s.",
		SizeAskSys:      "To publish something bigger, ask your system administrator.",
		SizeLabel:       "New limit",
		SizeHint:        "Up to %s, e.g. 500MB or 1.5GB. Leave empty to go back to the default (%s).",
		SizeSave:        "Change limit",
		SizeInvalid:     "Write the size as a number with MB or GB, e.g. 500MB or 1.5GB.",
		SizeTooHigh:     "The limit can be at most %s.",
		AccessSave:      "Save",
		AccessCodeLabel: "Access code",
		AccessCodeKeep:  "Leave empty to keep the current code",
		LevelTeam:       "Everyone at the company",
		LevelTeamHint:   "Anyone signed in with a company account.",
		LevelOnlyMe:     "Only me",
		LevelOnlyMeHint: "Just you, once signed in.",
		LevelPeople:     "Chosen people",
		LevelPeopleHint: "You and the people you add below.",
		LevelCode:       "With a code",
		LevelCodeHint:   "Anyone who has the access code.",
		LevelPublic:     "Public",
		LevelPublicHint: "Anyone with the link, no sign-in.",
		PeopleTitle:     "People who can see it",
		PeopleOnlyOwner: "For now, only you.",
		PeopleRemove:    "Remove",
		PeopleAdd:       "Add",
		PeopleEmail:     "Company email",
		PeopleEmailHint: "You can add several, separated by commas.",
		PeopleEmailUser: "name",

		AccessErrNotOwnerA:    "Only the owner",
		AccessErrNotOwnerB:    "can change who can see this site.",
		AccessErrNoOwner:      "This site has no owner on record, so it can't be limited to chosen people.",
		AccessErrBadEmail:     "this isn't a valid email address.",
		AccessErrMissingEmail: "Enter at least one email address.",
		AccessErrDomain:       "you can only add company accounts.",
		AccessErrTooMany:      "A site can be shared with at most 100 people.",
		AccessErrOwnerAlways:  "The owner can always see the site.",
		AccessErrCode:         "Choose an access code.",
		AccessErrGeneric:      "The change couldn't be saved.",

		RestrictedTitle:    "quick — restricted",
		RestrictedHeading:  "This site is restricted",
		RestrictedBy:       "Only people chosen by",
		RestrictedCanOpen:  "can open",
		RestrictedSignedIn: "You're signed in as",
		RestrictedAsk:      "Ask the owner to add you.",
		RestrictedSwitch:   "Sign in with another account",

		CodeTitle:   "Protected access",
		CodeHeading: "Protected site",
		CodeIntro:   "Enter the access code for",
		CodeLabel:   "Code",
		CodeButton:  "Enter",
		CodeError:   "Wrong code, try again.",

		NotFoundTitle:       "quick — not found",
		NotFoundSiteHeading: "Site not found",
		NotFoundSiteIntro:   "Nothing is published at",
		NotFoundPageHeading: "Page not found",
		NotFoundPageIntro:   "This page doesn't exist on",
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
		GuideVisibility: "Scegli chi può aprire il sito: SSO aziendale, persone scelte, codice di accesso o pubblico.",
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
		DashHelpDocs:    "Installazione della CLI e guide su",
		DashSearch:      "Cerca per nome",
		DashNoMatch:     "Nessun sito corrisponde.",
		NavDocs:         "Documentazione",
		GuideMore:       "Tutti i comandi e le guide",
		BadgePublic:     "pubblico",
		DashManage:      "Gestisci",

		SiteBack:        "Torna alla dashboard",
		SiteTitle:       "Gestisci il sito",
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
		BadgePeople:     "persone scelte",
		BadgeOnlyMe:     "solo io",

		AccessTitle:     "Chi può vedere il sito",
		AccessOwner:     "Proprietario:",
		AccessCurrent:   "Al momento:",
		AccessOwnerOnly: "Solo il proprietario può cambiarlo da qui.",
		AccessAdmins:    "Gli amministratori (%s) possono vedere e gestire tutti i siti, anche quelli limitati a persone scelte.",
		SizeTitle:       "Dimensione massima",
		SizeIntro:       "Ogni versione di questo sito può occupare fino a",
		SizeAsk:         "Per pubblicare qualcosa di più grande chiedi a un amministratore: %s.",
		SizeAskSys:      "Per pubblicare qualcosa di più grande chiedi a chi gestisce il server.",
		SizeLabel:       "Nuovo limite",
		SizeHint:        "Fino a %s, per esempio 500MB o 1.5GB. Lascia vuoto per tornare al valore predefinito (%s).",
		SizeSave:        "Cambia limite",
		SizeInvalid:     "Scrivi la dimensione come numero con MB o GB, per esempio 500MB o 1.5GB.",
		SizeTooHigh:     "Il limite può essere al massimo %s.",
		AccessSave:      "Salva",
		AccessCodeLabel: "Codice di accesso",
		AccessCodeKeep:  "Lascia vuoto per tenere il codice attuale",
		LevelTeam:       "Tutta l'azienda",
		LevelTeamHint:   "Chiunque acceda con un account aziendale.",
		LevelOnlyMe:     "Solo io",
		LevelOnlyMeHint: "Solo tu, dopo l'accesso.",
		LevelPeople:     "Persone scelte",
		LevelPeopleHint: "Tu e le persone che aggiungi qui sotto.",
		LevelCode:       "Con codice",
		LevelCodeHint:   "Chiunque abbia il codice di accesso.",
		LevelPublic:     "Pubblico",
		LevelPublicHint: "Chiunque abbia il link, senza accesso.",
		PeopleTitle:     "Persone che possono vederlo",
		PeopleOnlyOwner: "Per ora solo tu.",
		PeopleRemove:    "Rimuovi",
		PeopleAdd:       "Aggiungi",
		PeopleEmail:     "Email aziendale",
		PeopleEmailHint: "Puoi aggiungerne più di una, separate da virgole.",
		PeopleEmailUser: "nome",

		AccessErrNotOwnerA:    "Solo il proprietario",
		AccessErrNotOwnerB:    "può cambiare chi vede questo sito.",
		AccessErrNoOwner:      "Questo sito non ha un proprietario registrato, quindi non si può limitare a persone scelte.",
		AccessErrBadEmail:     "non è un indirizzo email valido.",
		AccessErrMissingEmail: "Inserisci almeno un indirizzo email.",
		AccessErrDomain:       "puoi aggiungere solo account aziendali.",
		AccessErrTooMany:      "Un sito si può condividere con al massimo 100 persone.",
		AccessErrOwnerAlways:  "Il proprietario può sempre vedere il sito.",
		AccessErrCode:         "Scegli un codice di accesso.",
		AccessErrGeneric:      "Non è stato possibile salvare la modifica.",

		RestrictedTitle:    "quick · accesso riservato",
		RestrictedHeading:  "Questo sito è riservato",
		RestrictedBy:       "Solo le persone scelte da",
		RestrictedCanOpen:  "possono aprire",
		RestrictedSignedIn: "Hai effettuato l'accesso come",
		RestrictedAsk:      "Chiedi al proprietario di aggiungerti.",
		RestrictedSwitch:   "Accedi con un altro account",

		CodeTitle:   "Accesso protetto",
		CodeHeading: "Sito protetto",
		CodeIntro:   "Inserisci il codice di accesso per",
		CodeLabel:   "Codice",
		CodeButton:  "Entra",
		CodeError:   "Codice errato, riprova.",

		NotFoundTitle:       "quick · non trovato",
		NotFoundSiteHeading: "Sito non trovato",
		NotFoundSiteIntro:   "Non c'è nessun sito pubblicato su",
		NotFoundPageHeading: "Pagina non trovata",
		NotFoundPageIntro:   "Questa pagina non esiste su",
	},
}

func textsFor(l lang) uiText {
	if t, ok := uiTexts[l]; ok {
		return t
	}
	return uiTexts[langEN]
}

// policyBadge maps a site's visibility to its localized dashboard badge.
func policyBadge(p policy, t uiText) string {
	switch accessKey(p) {
	case "public":
		return t.BadgePublic
	case "code":
		return t.BadgeCode
	case "only-me":
		return t.BadgeOnlyMe
	case "people":
		return t.BadgePeople
	default:
		return t.BadgeSSO
	}
}
