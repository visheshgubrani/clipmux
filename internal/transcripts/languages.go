package transcripts

import "strings"

// whisperLanguages are the languages Whisper detects, by the code whisper.cpp
// reports and the name OpenAI-compatible APIs report (openai/whisper
// tokenizer.py). Javanese is "jw" in Whisper and "jv" in ISO 639-1.
var whisperLanguages = map[string]string{
	"en": "english", "zh": "chinese", "de": "german", "es": "spanish", "ru": "russian",
	"ko": "korean", "fr": "french", "ja": "japanese", "pt": "portuguese", "tr": "turkish",
	"pl": "polish", "ca": "catalan", "nl": "dutch", "ar": "arabic", "sv": "swedish",
	"it": "italian", "id": "indonesian", "hi": "hindi", "fi": "finnish", "vi": "vietnamese",
	"he": "hebrew", "uk": "ukrainian", "el": "greek", "ms": "malay", "cs": "czech",
	"ro": "romanian", "da": "danish", "hu": "hungarian", "ta": "tamil", "no": "norwegian",
	"th": "thai", "ur": "urdu", "hr": "croatian", "bg": "bulgarian", "lt": "lithuanian",
	"la": "latin", "mi": "maori", "ml": "malayalam", "cy": "welsh", "sk": "slovak",
	"te": "telugu", "fa": "persian", "lv": "latvian", "bn": "bengali", "sr": "serbian",
	"az": "azerbaijani", "sl": "slovenian", "kn": "kannada", "et": "estonian", "mk": "macedonian",
	"br": "breton", "eu": "basque", "is": "icelandic", "hy": "armenian", "ne": "nepali",
	"mn": "mongolian", "bs": "bosnian", "kk": "kazakh", "sq": "albanian", "sw": "swahili",
	"gl": "galician", "mr": "marathi", "pa": "punjabi", "si": "sinhala", "km": "khmer",
	"sn": "shona", "yo": "yoruba", "so": "somali", "af": "afrikaans", "oc": "occitan",
	"ka": "georgian", "be": "belarusian", "tg": "tajik", "sd": "sindhi", "gu": "gujarati",
	"am": "amharic", "yi": "yiddish", "lo": "lao", "uz": "uzbek", "fo": "faroese",
	"ht": "haitian creole", "ps": "pashto", "tk": "turkmen", "nn": "nynorsk", "mt": "maltese",
	"sa": "sanskrit", "lb": "luxembourgish", "my": "myanmar", "bo": "tibetan", "tl": "tagalog",
	"mg": "malagasy", "as": "assamese", "tt": "tatar", "haw": "hawaiian", "ln": "lingala",
	"ha": "hausa", "ba": "bashkir", "jv": "javanese", "su": "sundanese", "yue": "cantonese",
}

// languageAliases are the other names Whisper accepts for a language.
var languageAliases = map[string]string{
	"burmese": "my", "valencian": "ca", "flemish": "nl", "haitian": "ht", "letzeburgesch": "lb",
	"pushto": "ps", "panjabi": "pa", "moldavian": "ro", "moldovan": "ro", "sinhalese": "si",
	"castilian": "es", "mandarin": "zh", "jw": "jv",
}

var languageNames = func() map[string]string {
	m := make(map[string]string, len(whisperLanguages)+len(languageAliases))
	for code, name := range whisperLanguages {
		m[name] = code
		m[code] = code
	}
	for alias, code := range languageAliases {
		m[alias] = code
	}
	return m
}()

// LanguageCode turns what a recogniser reported, a code ("hi") or a name
// ("Hindi"), into the transcript's language code, or "und" when it is
// unknown.
func LanguageCode(reported string) string {
	if code, ok := languageNames[strings.ToLower(strings.TrimSpace(reported))]; ok {
		return code
	}
	return "und"
}
