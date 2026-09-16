package app

import (
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var aiSecretPatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"a private key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
	{"an API key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)},
	{"a GitHub token", regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{"a Slack token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{"an AWS access key", regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"a Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{"a webhook or bot token", regexp.MustCompile(`(?i)hooks\.slack\.com/(services|workflows|triggers)/[A-Za-z0-9/_-]{20,}|discord(app)?\.com/api/webhooks/\d+/[A-Za-z0-9_-]{20,}|\b\d{6,12}:AA[A-Za-z0-9_-]{30,}`)},
	{"a JSON web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
}

// aiNamedPattern finds a password, secret, API key or token set right after
// its name: "password=x", "Пароль: x".
var aiNamedPattern = regexp.MustCompile(`(?i)(password|passwd|passphrase|pwd|secret|api[_ -]?key|access[_ -]?token|пароль|токен)\s*([:=])\s*(\S+)`)

// aiNamedPatternFound reports whether text sets a credential right after its
// name. After a colon, a word that reads as an instruction is not the value,
// as in "Пароль: забыла, сходить в банк"; after "=", any word is.
func aiNamedPatternFound(text string) bool {
	for _, m := range aiNamedPattern.FindAllStringSubmatch(text, -1) {
		value := strings.TrimFunc(m[3], func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		if m[2] != ":" || !aiNotValue(value) {
			return true
		}
	}
	return false
}

const (
	aiTokenMinLength  = 32
	aiTokenMinEntropy = 3.5
)

// AILeftOut counts what a call kept out of its prompt because AISecretKind
// flagged it: cached tasks by their titles, and the names of lists and tags.
// It is a count only; what was left out is never repeated.
type AILeftOut struct {
	Tasks int `json:"tasks"`
	Lists int `json:"lists"`
	Tags  int `json:"tags"`
}

// aiWithheld collects what a call left out, keyed by task ID, list ID and
// lowercase tag name, so nothing is counted twice.
type aiWithheld struct {
	tasks, lists, tags map[string]bool
}

func newAIWithheld() aiWithheld {
	return aiWithheld{tasks: map[string]bool{}, lists: map[string]bool{}, tags: map[string]bool{}}
}

func (w aiWithheld) count() AILeftOut {
	return AILeftOut{Tasks: len(w.tasks), Lists: len(w.lists), Tags: len(w.tags)}
}

// AISecretKind names what in text looks like a credential, or returns "" when
// nothing does. It never returns the matched text itself.
//
// Besides the formats of well-known keys and tokens and long random-looking
// strings, it finds a card number, a password or token inside a link, and a
// credential written a few words after its name, as in "wifi password
// hunter2", "PIN 4821", "OTP 482913", "пароль от wifi qwerty123" or "Сменить
// пароль для роутера на Qwerty123"; right after a colon that follows the name,
// any word but an instruction counts, as in "пароль от почты: солнышко". A
// name with no value after it is not one: "PIN code reminder", "Проверить
// пароль политику" and "Ключ: забрать у консьержа" pass. Neither is a date, a
// time, a measure such as 17mm or 2FA, a year after a password word (unless a
// colon introduces it), a number of a length the name does not take, a
// #reference, the account a password is reset for, the share and campaign IDs
// sites add to links, nor a name used in its ordinary sense: a boarding pass,
// an order code, the key to a safe deposit box. So "Сменить пароль до 15
// октября", "Reset password for user123" and "Код заказа 123456" pass too.
func AISecretKind(text string) string {
	for _, p := range aiSecretPatterns {
		if p.re.MatchString(text) {
			return p.kind
		}
	}
	if aiNamedPatternFound(text) {
		return "a password or secret"
	}
	// Digits inside a link are an ID of the page, not a card.
	if aiHasCardNumber(aiURLRe.ReplaceAllString(text, " ")) {
		return "a card number"
	}
	// A whole word is read as a link first, since a password in a link may hold
	// the brackets and quotes that end a link found in running text.
	for _, word := range strings.Fields(text) {
		if strings.Contains(word, "://") {
			if kind := aiURLSecretKind(strings.TrimFunc(word, aiEdgePunct)); kind != "" {
				return kind
			}
		}
	}
	for _, link := range aiURLRe.FindAllString(text, -1) {
		link = strings.TrimRight(link, ".,;:!?")
		if !strings.Contains(link, "://") {
			link = "https://" + link
		}
		if kind := aiURLSecretKind(link); kind != "" {
			return kind
		}
	}
	if kind := aiNamedCredential(text); kind != "" {
		return kind
	}
	for _, word := range strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(`"'()[]{}<>,;`, r)
	}) {
		if aiLooksLikeToken(word, aiTokenMinLength) {
			return "a long random-looking token"
		}
	}
	return ""
}

// aiURLRe finds links in text: with a scheme, or as a host and a path that
// carries a query or a fragment. A link ends at whitespace and at the
// brackets and quotes around it, as in markdown.
var aiURLRe = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s<>"'()\[\]{}]+|\b[a-z0-9-]+(\.[a-z0-9-]+)+/[^\s<>"'()\[\]{}]*[?#][^\s<>"'()\[\]{}]+`)

// aiURLCredentialParams are query parameters whose value is a credential,
// whatever it looks like.
var aiURLCredentialParams = map[string]bool{"token": true, "access_token": true, "refresh_token": true, "id_token": true,
	"auth": true, "auth_token": true, "key": true, "api_key": true, "apikey": true, "secret": true, "client_secret": true,
	"password": true, "pass": true, "pwd": true, "sig": true, "signature": true}

// aiURLTrackingParam reports whether a query parameter carries the ID of a
// share, a click or a campaign that sites add to links, not a credential.
func aiURLTrackingParam(name string) bool {
	switch name {
	case "si", "igsh", "igshid", "fbclid", "gclid", "gbraid", "wbraid", "msclkid", "yclid", "dclid",
		"ttclid", "twclid", "mc_eid", "g_ep", "atlorigin", "list", "asb", "share_id", "rcm", "xmt", "spm",
		"source_impression_id", "do-waremd5":
		return true
	}
	return strings.HasPrefix(name, "utm_") || strings.HasPrefix(name, "pd_rd_") || strings.HasPrefix(name, "pf_rd_")
}

// aiURLShareParams are the parameters that, on a site's own links, name what
// is shared rather than grant access: the file or slide of a Google Drive
// link, which its path form carries in the open as well, the share markers of
// Figma, X and Loom, a GitLab commit, and the listing context of Avito.
var aiURLShareParams = map[string]map[string]bool{
	"google.com":  {"id": true, "slide": true},
	"figma.com":   {"t": true},
	"x.com":       {"t": true},
	"twitter.com": {"t": true},
	"gitlab.com":  {"commit_id": true},
	"loom.com":    {"sid": true},
	"avito.ru":    {"context": true},
}

// aiURLQueryTokenLength is how long a query value must be to be judged as a
// random-looking token.
const aiURLQueryTokenLength = 16

// aiURLSecretKind names the credential a link carries: a password in its user
// part, a value of a parameter named for a credential, or a long
// random-looking value, in its query or fragment.
func aiURLSecretKind(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	if u.User != nil {
		if password, ok := u.User.Password(); ok && password != "" {
			return "a password or secret"
		}
	}
	var shared map[string]bool
	host := strings.ToLower(u.Hostname())
	for site, params := range aiURLShareParams {
		if host == site || strings.HasSuffix(host, "."+site) {
			shared = params
		}
	}
	fragment, _ := url.ParseQuery(u.Fragment)
	for _, query := range []url.Values{u.Query(), fragment} {
		for name, values := range query {
			name = strings.ToLower(name)
			if aiURLTrackingParam(name) || shared[name] {
				continue
			}
			for _, value := range values {
				switch {
				case value != "" && aiURLCredentialParams[name]:
					return "a key or token"
				case aiLooksLikeToken(value, aiURLQueryTokenLength):
					return "a long random-looking token"
				}
			}
		}
	}
	return ""
}

// aiCredential is what a word names when it names a credential: the kind
// AISecretKind reports; how many digits alone are such a value, from
// minDigits to maxDigits (0: no limit), and whether a year among them is one
// without a colon to introduce it; and how long any other value is at least.
type aiCredential struct {
	kind                 string
	minDigits, maxDigits int
	years                bool
	shortest             int

	// plainAfterColon keeps the usual test for a value after a colon: a
	// boarding pass or a ski pass is labelled as often as it is secret.
	plainAfterColon bool
}

var (
	aiPassword = aiCredential{kind: "a password or secret", minDigits: 4, shortest: 4}
	aiPIN      = aiCredential{kind: "a PIN or code", minDigits: 4, maxDigits: 8, years: true, shortest: 4}
	aiSentCode = aiCredential{kind: "a PIN or code", minDigits: 4, maxDigits: 8, shortest: 4}
	aiOTP      = aiCredential{kind: "a PIN or code", minDigits: 4, maxDigits: 10, shortest: 4}
	aiCardCode = aiCredential{kind: "a card security code", minDigits: 3, maxDigits: 4, years: true, shortest: 3}
	aiKey      = aiCredential{kind: "a key or token", minDigits: 6, shortest: 8}
)

// takesDigits reports whether the digits d alone are a value of c.
func (c aiCredential) takesDigits(d string, afterColon bool) bool {
	return len(d) >= c.minDigits && (c.maxDigits == 0 || len(d) <= c.maxDigits) && (c.years || afterColon || !aiYear(d))
}

// aiYear reports whether four digits read as a year of this era or the last.
func aiYear(d string) bool {
	return len(d) == 4 && (strings.HasPrefix(d, "19") || strings.HasPrefix(d, "20"))
}

// aiCredentialWords name a credential by themselves.
var aiCredentialWords = map[string]aiCredential{
	"password": aiPassword, "passwords": aiPassword, "passwd": aiPassword, "passphrase": aiPassword,
	"passcode": aiPassword, "pwd": aiPassword, "pw": aiPassword,
	"pin": aiPIN, "pincode": aiPIN, "otp": aiOTP, "totp": aiOTP,
	"cvv": aiCardCode, "cvv2": aiCardCode, "cvc": aiCardCode, "cvc2": aiCardCode,
	"token": aiKey, "tokens": aiKey, "secret": aiKey, "secrets": aiKey, "apikey": aiKey, "api-key": aiKey, "api_key": aiKey,
}

// aiKeyQualifiers and aiCodeQualifiers make "key" and "code" the name of a
// credential when they come right before it: an access key or a Wi-Fi key is
// one, a key alone need not be. A code sent to confirm a login, a one-time
// code and a backup code take no year, and the last two run to ten digits.
// aiWordQualifiers do the same for a secret word. aiPassQualifiers make "pass"
// an ordinary pass instead.
var (
	aiKeyQualifiers = map[string]bool{"api": true, "access": true, "secret": true, "private": true, "license": true,
		"licence": true, "product": true, "activation": true, "recovery": true,
		"wifi": true, "wi-fi": true, "wpa": true, "wpa2": true, "wpa3": true, "wlan": true, "wireless": true, "network": true}
	aiCodeQualifiers = map[string]aiCredential{"pin": aiPIN, "access": aiPIN, "door": aiPIN, "gate": aiPIN, "alarm": aiPIN,
		"entry": aiPIN, "security": aiPIN, "unlock": aiPIN,
		"verification": aiSentCode, "confirmation": aiSentCode, "sms": aiSentCode, "login": aiSentCode, "authentication": aiSentCode,
		"backup": aiOTP, "recovery": aiOTP, "2fa": aiOTP, "mfa": aiOTP, "one-time": aiOTP}
	aiWordQualifiers = map[string]bool{"secret": true, "code": true, "секретное": true, "кодовое": true, "контрольное": true}
	aiPassQualifiers = map[string]bool{"boarding": true, "season": true, "bus": true, "ski": true, "day": true,
		"gym": true, "train": true, "metro": true, "parking": true, "museum": true, "festival": true, "guest": true,
		"visitor": true, "press": true, "travel": true, "transit": true, "weekly": true, "monthly": true, "annual": true,
		"conference": true, "event": true, "concert": true, "hotel": true, "lift": true, "pool": true, "zoo": true}
)

// aiResetVerbs, before a password's name, make what follows "for" the account
// whose password it is, not the password; aiResetFillers may stand between.
var (
	aiResetVerbs = map[string]bool{"reset": true, "change": true, "update": true, "recover": true, "restore": true,
		"сбросить": true, "сменить": true, "поменять": true, "обновить": true, "восстановить": true}
	aiResetFillers = map[string]bool{"the": true, "my": true, "your": true, "our": true, "a": true,
		"мой": true, "свой": true, "мои": true, "свои": true}
)

// aiAccountWords may stand before the account a password is reset for.
var aiAccountWords = map[string]bool{"the": true, "my": true, "your": true, "our": true, "a": true, "an": true,
	"user": true, "account": true, "мой": true, "моего": true, "пользователя": true, "аккаунта": true, "учётки": true, "учетки": true}

// aiPassIntro, right after a "pass" that opens a title, make it a password,
// as in "pass от роутера Kv45_dacha".
var aiPassIntro = map[string]bool{"от": true, "для": true, "к": true, "на": true, "for": true, "to": true, "of": true,
	"from": true, "wifi": true, "wi-fi": true, "вайфай": true, "router": true, "роутера": true}

// aiValueStopWords are words that look like a value and are the name of
// something else.
var aiValueStopWords = map[string]bool{"1password": true}

// aiNotValues are words that, right after a colon, say what to do about a
// credential or where it is, not what it is: to-do verbs and imperatives,
// negations, and short function words.
var aiNotValues = map[string]bool{"ask": true, "call": true, "get": true, "set": true, "renew": true,
	"reset": true, "change": true, "update": true, "check": true, "find": true, "send": true, "pick": true,
	"buy": true, "remember": true, "request": true, "pending": true, "tbd": true, "todo": true, "unknown": true,
	"forgot": true, "lost": true, "none": true, "n/a": true, "not": true, "see": true, "under": true, "with": true,
	"from": true, "for": true, "the": true, "and": true, "here": true, "there": true,
	"не": true, "нет": true, "забыл": true, "забыла": true, "забыли": true, "потерял": true, "потеряла": true,
	"потеряли": true, "спроси": true, "позвони": true, "уточни": true, "узнай": true, "напиши": true, "забери": true,
	"проверь": true, "под": true, "над": true, "при": true, "для": true, "без": true, "через": true, "около": true,
	"возле": true, "или": true, "это": true, "там": true, "тут": true, "здесь": true, "где": true, "как": true}

// aiInfinitive matches a Russian infinitive: забрать, спросить, помочь.
var aiInfinitive = regexp.MustCompile(`[^с]ть(ся)?$|ти(сь)?$|чь(ся)?$`)

// aiNotValue reports whether a word right after a colon reads as an
// instruction, a negation or a function word rather than a value.
func aiNotValue(core string) bool {
	lower := strings.ToLower(core)
	n := utf8.RuneCountInString(lower)
	return n < 3 || aiNotValues[lower] || n > 3 && aiInfinitive.MatchString(lower)
}

// aiCredentialForms name a credential in the forms a word takes. unless holds
// the words that, right after it, make the name an ordinary one, as an error
// code or an order code is.
var aiCredentialForms = []struct {
	re         *regexp.Regexp
	credential aiCredential
	unless     map[string]bool
}{
	{regexp.MustCompile(`^парол\p{L}*$`), aiPassword, nil},
	{regexp.MustCompile(`^(pin|пин)-?(code|код\p{L}*)$|^пин(а|у|ом|е)?$`), aiPIN, nil},
	{regexp.MustCompile(`^код(а|у|ом|е|ы|ов|ам|ами|ах)?$|^кодов\p{L}*$`), aiPIN,
		map[string]bool{"ошибки": true, "ошибка": true, "товара": true, "региона": true, "заказа": true, "заказов": true,
			"отслеживания": true, "страны": true, "города": true}},
	{regexp.MustCompile(`^токен\p{L}*$|^ключ(а|у|ом|е|и|ей|ам|ами|ах)?$|^секрет(а|у|ом|е|ы|ов|ам|ами|ах)?$`), aiKey, nil},
}

// aiCredentialWindow is how many words after a credential's name its value is
// looked for; a word ending in a colon lets the look go one word further, up
// to twice as far.
const aiCredentialWindow = 4

// aiNamedCredential returns the kind of the first credential in text written
// after its name, or "".
func aiNamedCredential(text string) string {
	words := strings.Fields(text)
	for i := range words {
		c, ok := aiCredentialNamed(words, i)
		if !ok {
			continue
		}
		reset := aiResetBefore(words, i)
		base := i
		for j := i + 1; j < len(words); j++ {
			afterColon := strings.HasSuffix(words[j-1], ":")
			if j > base+aiCredentialWindow && (!afterColon || j > base+2*aiCredentialWindow) {
				// A long label such as "пароль для входа в личный кабинет:"
				// is looked through to its colon, still at most twice as far.
				if j < base+2*aiCredentialWindow && base == i && aiNamePrepositions[aiWordKey(words[i+1])] {
					continue
				}
				break
			}
			if key := aiWordKey(words[j]); reset && (key == "for" || key == "для") {
				// The account after "for", a few words at most, is not the
				// password; a new one after "to", "на" or a colon still is, and
				// the look starts again there - once, so a long text is still
				// looked through in a single pass.
				reset = false
				account := false
				for skipped := 0; skipped < aiCredentialWindow && j+1 < len(words) && !strings.HasSuffix(words[j], ":"); skipped++ {
					next := aiWordKey(words[j+1])
					if next == "to" || next == "на" || account && aiValueLike(c, words[j+1], false) {
						break
					}
					account = account || !aiAccountWords[next]
					j++
				}
				base = j
				continue
			}
			if aiValueLike(c, words[j], afterColon && !c.plainAfterColon && (aiColonOnName(words, i, j-1) || base > i || j == len(words)-1)) {
				return c.kind
			}
		}
	}
	return ""
}

// aiColonOnName reports whether the colon ending words[k] closes the name of
// the credential at i, or a phrase after it that opens with a preposition, as
// in "пароль от почты:", rather than a topic such as "Password reset:".
func aiColonOnName(words []string, i, k int) bool {
	return k == i || k > i && aiNamePrepositions[aiWordKey(words[i+1])]
}

var aiNamePrepositions = map[string]bool{"от": true, "для": true, "к": true, "на": true, "у": true, "в": true,
	"for": true, "to": true, "of": true, "from": true, "at": true, "in": true, "on": true, "is": true}

// aiResetBefore reports whether a reset verb comes before the password's name
// at i, with at most one filler such as "the" or "мой" between.
func aiResetBefore(words []string, i int) bool {
	for k := i - 1; k >= 0 && k >= i-2; k-- {
		key := aiWordKey(words[k])
		if aiResetVerbs[key] {
			return true
		}
		if !aiResetFillers[key] {
			return false
		}
	}
	return false
}

func aiCredentialNamed(words []string, i int) (aiCredential, bool) {
	word := aiWordKey(words[i])
	if c, ok := aiCredentialWords[word]; ok {
		return c, true
	}
	prev, next := "", ""
	if i > 0 {
		prev = aiWordKey(words[i-1])
	}
	if i+1 < len(words) {
		next = aiWordKey(words[i+1])
	}
	// An SMS code is commonly named in one hyphenated word, as in
	// "SMS-code 482913", and in Russian above all.
	if q, name, ok := strings.Cut(word, "-"); ok && (q == "sms" || q == "смс") && (name == "code" || name == "codes" || name == "код") {
		return aiCodeQualifiers["sms"], true
	}
	switch word {
	case "key", "keys":
		return aiKey, aiKeyQualifiers[prev]
	case "code", "codes":
		c, ok := aiCodeQualifiers[prev]
		return c, ok
	case "word", "слово":
		return aiPassword, aiWordQualifiers[prev]
	case "pass":
		// A title that opens with "pass" means the verb, as in "Pass the
		// exam", and a boarding pass or a ski pass is a pass - unless a colon,
		// "is" or, in a title that opens with it, a word such as "от" or
		// "wifi" says what it holds.
		c := aiPassword
		c.plainAfterColon = aiPassQualifiers[prev]
		return c, strings.HasSuffix(words[i], ":") || next == "is" || i > 0 && !aiPassQualifiers[prev] ||
			i == 0 && (aiPassIntro[next] || i+1 < len(words) && strings.HasSuffix(words[i+1], ":"))
	}
	for _, form := range aiCredentialForms {
		if form.re.MatchString(word) {
			return form.credential, !form.unless[next]
		}
	}
	return aiCredential{}, false
}

// aiWordKey is a word as a name is looked up: lower case, without the quotes,
// brackets and sentence punctuation around it.
func aiWordKey(word string) string {
	return strings.ToLower(strings.TrimFunc(word, aiEdgePunct))
}

func aiEdgePunct(r rune) bool {
	return unicode.In(r, unicode.Ps, unicode.Pe, unicode.Pi, unicode.Pf) || strings.ContainsRune(`"'.,;:!?`, r)
}

// aiMeasure matches a number that is a date, a time or a measure: 15.10,
// 10:30, 17mm, 2FA, 5pm.
var aiMeasure = regexp.MustCompile(`^\d+([.,:]\d+)*\p{L}{0,3}$`)

// aiValueLike reports whether word, found after the name of the credential c,
// looks like its value: letters with digits or symbols, letters whose case
// keeps changing, or digits alone of a length c takes. Right after a colon,
// any word that is not a date, a time, a measure or an instruction counts. A
// #reference never does.
func aiValueLike(c aiCredential, word string, afterColon bool) bool {
	core := strings.TrimFunc(word, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if core == "" || strings.HasPrefix(word, "#") || aiValueStopWords[strings.ToLower(core)] {
		return false
	}
	if strings.Trim(core, "0123456789") == "" {
		return c.takesDigits(core, afterColon)
	}
	if aiMeasure.MatchString(core) {
		return false
	}
	if afterColon && !aiNotValue(core) {
		return true
	}
	value := strings.TrimFunc(word, aiEdgePunct)
	if utf8.RuneCountInString(value) < c.shortest {
		return false
	}
	var letter, digit, symbol bool
	changes, lower := 0, false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r):
			letter = true
			if lower && unicode.IsUpper(r) {
				changes++
			}
			lower = unicode.IsLower(r)
			continue
		case unicode.IsDigit(r):
			digit = true
		case strings.ContainsRune(`!@#$%^&*_+=~|\`, r):
			symbol = true
		}
		lower = false
	}
	return letter && (digit || symbol) || changes >= 2
}

// aiHasCardNumber reports whether text holds a payment card number: 13 to 19
// digits written as one number, with single spaces or dashes between groups,
// that pass the Luhn check or are printed in groups of four. A number glued to
// letters is some other code, and one after a plus sign is a phone number.
func aiHasCardNumber(text string) bool {
	letterOrDigit := func(b byte) bool { return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			continue
		}
		start := i
		var digits []byte
		groups := []int{0}
		for i < len(text) {
			if text[i] >= '0' && text[i] <= '9' {
				digits = append(digits, text[i])
				groups[len(groups)-1]++
				i++
				continue
			}
			if (text[i] == ' ' || text[i] == '-') && i+1 < len(text) && text[i+1] >= '0' && text[i+1] <= '9' {
				groups = append(groups, 0)
				i++
				continue
			}
			break
		}
		if start > 0 && (text[start-1] == '+' || letterOrDigit(text[start-1])) || i < len(text) && letterOrDigit(text[i]) {
			continue
		}
		if n := len(digits); n >= 13 && n <= 19 && (slices.Min(groups) >= 4 && aiLuhn(digits) || aiGroupedByFour(groups)) {
			return true
		}
	}
	return false
}

func aiLuhn(digits []byte) bool {
	sum := 0
	for i := range digits {
		d := int(digits[len(digits)-1-i] - '0')
		if i%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

// aiGroupedByFour reports whether digit groups are laid out the way cards
// print their numbers: fours with a shorter last group, or 4-6-5.
func aiGroupedByFour(groups []int) bool {
	if len(groups) == 3 {
		return groups[0] == 4 && groups[1] == 6 && groups[2] == 5
	}
	if len(groups) < 4 {
		return false
	}
	for _, n := range groups[:len(groups)-1] {
		if n != 4 {
			return false
		}
	}
	return groups[len(groups)-1] <= 4
}

// aiLooksLikeToken reports whether word, at least minLength long, is a mix of
// letters and digits random enough to be a token.
func aiLooksLikeToken(word string, minLength int) bool {
	if len(word) < minLength {
		return false
	}
	letters, digits := false, false
	counts := map[rune]int{}
	for _, r := range word {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
			letters = true
		case r >= '0' && r <= '9':
			digits = true
		case strings.ContainsRune("+/=_-.", r):
		default:
			return false
		}
		counts[r]++
	}
	if !letters || !digits {
		return false
	}
	entropy := 0.0
	for _, n := range counts {
		p := float64(n) / float64(len(word))
		entropy -= p * math.Log2(p)
	}
	return entropy >= aiTokenMinEntropy
}
