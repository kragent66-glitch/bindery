import i18n from 'i18next'
import { initReactI18next } from 'react-i18next'
import LanguageDetector from 'i18next-browser-languagedetector'

import en from './locales/en.json'
import fr from './locales/fr.json'
import de from './locales/de.json'
import es from './locales/es.json'
import nl from './locales/nl.json'
import tl from './locales/tl.json'
import id from './locales/id.json'
import ko from './locales/ko.json'
import nb from './locales/nb.json'
import sv from './locales/sv.json'

// Region tags need no mapping: i18next tries `de-DE`, then `de`, so `nb-NO`,
// `sv-SE` and `sv-FI` already land on `nb` and `sv`. Norwegian is the one case
// where the browser's base tag is not the bundle's: `no` is the macrolanguage
// tag some browsers send, and `nn` is Nynorsk, which has no bundle of its own.
// Nynorsk readers read Bokmål routinely (it is most of what Norwegian software
// ships in), so both fall back to `nb` before English. The detected tag itself
// is kept, so dates still format in the reader's own variant.
export const FALLBACK_LNG = {
  no: ['nb', 'en'],
  nn: ['nb', 'en'],
  default: ['en'],
}

// Reads from localStorage key 'bindery.lang' first, then falls back to the
// browser's navigator.language. This mirrors the theme bootstrap so the first
// paint is already in the right language — no flash of English.
i18n
  .use(LanguageDetector)
  .use(initReactI18next)
  .init({
    resources: {
      en: { translation: en },
      fr: { translation: fr },
      de: { translation: de },
      es: { translation: es },
      nl: { translation: nl },
      tl: { translation: tl },
      id: { translation: id },
      ko: { translation: ko },
      nb: { translation: nb },
      sv: { translation: sv },
    },
    fallbackLng: FALLBACK_LNG,
    detection: {
      order: ['localStorage', 'navigator'],
      lookupLocalStorage: 'bindery.lang',
      caches: ['localStorage'],
    },
    interpolation: {
      escapeValue: false, // React already escapes output
    },
  })

export default i18n
