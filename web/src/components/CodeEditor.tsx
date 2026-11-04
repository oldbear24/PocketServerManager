import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { yaml } from "@codemirror/lang-yaml";
import { HighlightStyle, indentUnit, syntaxHighlighting } from "@codemirror/language";
import { lintGutter, setDiagnostics, type Diagnostic } from "@codemirror/lint";
import {
  Compartment,
  EditorState,
  RangeSetBuilder,
  StateEffect,
  StateField,
} from "@codemirror/state";
import { Decoration, EditorView, keymap, lineNumbers, type DecorationSet } from "@codemirror/view";
import { tags } from "@lezer/highlight";
import { useEffect, useRef } from "react";

import type { StackProblem } from "../lib/stacks";

const theme = EditorView.theme(
  {
    "&": {
      backgroundColor: "transparent",
      color: "var(--text)",
      fontSize: "13px",
    },
    "&.cm-focused": { outline: "none" },
    ".cm-content": {
      fontFamily: "var(--font-mono)",
      caretColor: "var(--text)",
      padding: "10px 0",
    },
    ".cm-gutters": {
      backgroundColor: "transparent",
      color: "var(--text-3)",
      border: "none",
    },
    ".cm-activeLine": { backgroundColor: "transparent" },
    ".cm-cursor": { borderLeftColor: "var(--text)" },
    ".cm-selectionBackground, &.cm-focused .cm-selectionBackground": {
      backgroundColor: "var(--accent-bg) !important",
    },
    ".cm-line-added": { backgroundColor: "var(--ok-bg)" },
    ".cm-lintRange-error": { textDecorationColor: "var(--err)" },
    ".cm-tooltip": {
      backgroundColor: "var(--surface-2)",
      border: "1px solid var(--border-control)",
      color: "var(--text)",
    },
  },
  { dark: true },
);

const highlight = HighlightStyle.define([
  { tag: tags.propertyName, color: "#8fb0ff" },
  { tag: tags.definition(tags.propertyName), color: "#8fb0ff" },
  { tag: [tags.string, tags.content], color: "#d6dae0" },
  { tag: [tags.number, tags.bool, tags.null], color: "#7be3a2" },
  { tag: tags.comment, color: "#7e8794" },
  { tag: [tags.meta, tags.punctuation, tags.separator], color: "#a7afbb" },
]);

const setAddedLines = StateEffect.define<number[]>();

const addedLineMark = Decoration.line({ class: "cm-line-added" });

/** Marks the lines that are new compared with the saved revision. */
const addedLinesField = StateField.define<DecorationSet>({
  create: () => Decoration.none,
  update(marks, transaction) {
    let next = marks.map(transaction.changes);
    for (const effect of transaction.effects) {
      if (effect.is(setAddedLines)) {
        const builder = new RangeSetBuilder<Decoration>();
        for (const number of effect.value) {
          if (number >= 1 && number <= transaction.state.doc.lines) {
            const line = transaction.state.doc.line(number);
            builder.add(line.from, line.from, addedLineMark);
          }
        }
        next = builder.finish();
      }
    }
    return next;
  },
  provide: (field) => EditorView.decorations.from(field),
});

function diagnosticsOf(state: EditorState, problems: StackProblem[]): Diagnostic[] {
  return problems
    .filter((problem) => problem.line && problem.line <= state.doc.lines)
    .map((problem): Diagnostic => {
      const line = state.doc.line(problem.line ?? 1);
      return {
        from: line.from,
        to: line.to,
        severity: "error",
        message: problem.message,
      };
    });
}

/**
 * The editor for Compose files. It is loaded only where it is needed: CodeMirror is
 * larger than the rest of a page.
 */
export default function CodeEditor({
  value,
  onChange,
  label,
  language,
  problems = [],
  added = [],
  readOnly = false,
}: {
  value: string;
  onChange: (value: string) => void;
  label: string;
  language: "yaml" | "env";
  problems?: StackProblem[];
  added?: number[];
  readOnly?: boolean;
}) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const latestChange = useRef(onChange);
  const editable = useRef(new Compartment());

  useEffect(() => {
    latestChange.current = onChange;
  }, [onChange]);

  useEffect(() => {
    if (!host.current) {
      return;
    }

    const editor = new EditorView({
      parent: host.current,
      state: EditorState.create({
        doc: value,
        extensions: [
          lineNumbers(),
          history(),
          keymap.of([indentWithTab, ...defaultKeymap, ...historyKeymap]),
          indentUnit.of("  "),
          language === "yaml" ? yaml() : [],
          syntaxHighlighting(highlight),
          lintGutter(),
          addedLinesField,
          theme,
          editable.current.of(EditorState.readOnly.of(readOnly)),
          EditorView.contentAttributes.of({ "aria-label": label }),
          EditorView.updateListener.of((update) => {
            if (update.docChanged) {
              latestChange.current(update.state.doc.toString());
            }
          }),
        ],
      }),
    });

    view.current = editor;

    return () => {
      editor.destroy();
      view.current = null;
    };
    // one editor per file; what changes later is applied below
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [language, label]);

  // a change from outside, such as loading a revision, replaces the text
  useEffect(() => {
    const editor = view.current;
    if (!editor || editor.state.doc.toString() === value) {
      return;
    }
    editor.dispatch({ changes: { from: 0, to: editor.state.doc.length, insert: value } });
  }, [value]);

  useEffect(() => {
    view.current?.dispatch({
      effects: editable.current.reconfigure(EditorState.readOnly.of(readOnly)),
    });
  }, [readOnly]);

  useEffect(() => {
    const editor = view.current;
    if (editor) {
      editor.dispatch(setDiagnostics(editor.state, diagnosticsOf(editor.state, problems)));
    }
  }, [problems, value]);

  useEffect(() => {
    view.current?.dispatch({ effects: setAddedLines.of(added) });
  }, [added]);

  return <div className="code-editor" ref={host} />;
}
