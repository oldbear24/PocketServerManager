import i18n from "i18next";
import LanguageDetector from "i18next-browser-languagedetector";
import { initReactI18next } from "react-i18next";

import cs from "./cs.json";
import en from "./en.json";

export const languages = ["en", "cs"] as const;

void i18n
  .use(LanguageDetector)
  .use(initReactI18next)
  .init({
    resources: { en: { translation: en }, cs: { translation: cs } },
    fallbackLng: "en",
    supportedLngs: languages,
    nonExplicitSupportedLngs: true,
    interpolation: { escapeValue: false }, // React escapes
    detection: {
      order: ["localStorage", "navigator"],
      lookupLocalStorage: "pocketcloud.language",
      caches: ["localStorage"],
    },
  });

i18n.on("languageChanged", (language) => {
  document.documentElement.lang = language;
});

export default i18n;
