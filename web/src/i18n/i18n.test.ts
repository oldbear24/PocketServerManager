import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import cs from "./cs.json";
import en from "./en.json";

type Tree = { [key: string]: string | Tree };

function flatten(tree: Tree, prefix = ""): string[] {
  return Object.entries(tree).flatMap(([key, value]) =>
    typeof value === "string" ? [prefix + key] : flatten(value, `${prefix}${key}.`),
  );
}

/** Plural forms differ per language, so keys are compared without them. */
function withoutPlurals(keys: string[]): Set<string> {
  return new Set(keys.map((key) => key.replace(/_(one|few|many|other)$/, "")));
}

function sourceFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) {
      return sourceFiles(path);
    }
    return /\.tsx?$/.test(name) && !name.includes(".test.") ? [path] : [];
  });
}

const enKeys = withoutPlurals(flatten(en));
const csKeys = withoutPlurals(flatten(cs));

describe("translations", () => {
  it("both languages have the same keys", () => {
    expect([...enKeys].filter((key) => !csKeys.has(key))).toEqual([]);
    expect([...csKeys].filter((key) => !enKeys.has(key))).toEqual([]);
  });

  it("every key used in the code exists", () => {
    const missing: string[] = [];

    for (const file of sourceFiles("src")) {
      const source = readFileSync(file, "utf8");
      for (const match of source.matchAll(/\bt\("([\w.]+)"/g)) {
        if (!enKeys.has(match[1])) {
          missing.push(`${file}: ${match[1]}`);
        }
      }
    }

    expect(missing).toEqual([]);
  });

  it("no value is empty", () => {
    const empty = (tree: Tree, prefix = ""): string[] =>
      Object.entries(tree).flatMap(([key, value]) =>
        typeof value === "string"
          ? value.trim() === ""
            ? [prefix + key]
            : []
          : empty(value, `${prefix}${key}.`),
      );

    expect(empty(en)).toEqual([]);
    expect(empty(cs)).toEqual([]);
  });

  it("plurals cover every form of Czech", () => {
    const keys = flatten(cs);
    const bases = new Set(
      keys
        .filter((key) => /_(one|few|many|other)$/.test(key))
        .map((key) => key.replace(/_\w+$/, "")),
    );

    for (const base of bases) {
      for (const form of ["one", "few", "many", "other"]) {
        expect(keys).toContain(`${base}_${form}`);
      }
    }
  });
});
