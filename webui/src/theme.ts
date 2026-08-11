/**
 * The committed light theme, ANSI 16 in Tango values - the terminal's own
 * colour vocabulary, so a tool that is cyan in the TUI is cyan here. The
 * values live in styles.css; this module is the one place that names them, so
 * canvas and DOM cannot drift apart.
 */

/** CSS custom property name for each tool id in model.Tool. */
const TOOL_VARS: Record<string, string> = {
  'claude-code': '--t-claude',
  codex: '--t-codex',
  opencode: '--t-open',
  copilot: '--t-copilot',
  hermes: '--t-hermes',
  gemini: '--t-gemini',
  agy: '--t-agy',
};

const UNKNOWN_TOOL_VAR = '--t-unknown';

export function toolColorVar(tool: string): string {
  return TOOL_VARS[tool] ?? UNKNOWN_TOOL_VAR;
}

/**
 * Resolved custom properties, read once per draw pass. getComputedStyle is a
 * layout read; doing it per block per frame is how a canvas scene ends up
 * costing more than the DOM it replaced.
 */
export class Palette {
  private readonly computed: CSSStyleDeclaration;
  private readonly cache = new Map<string, string>();

  constructor(element: Element = document.documentElement) {
    this.computed = getComputedStyle(element);
  }

  get(name: string): string {
    const hit = this.cache.get(name);
    if (hit !== undefined) return hit;
    const value = this.computed.getPropertyValue(name).trim();
    this.cache.set(name, value);
    return value;
  }

  tool(tool: string): string {
    return this.get(toolColorVar(tool));
  }
}
