// @vitest-environment jsdom
import {readFileSync} from 'node:fs';
import {describe, expect, it} from 'vitest';
import {DROP_TARGET_ACTIVE_CLASS, DROP_TARGET_ATTR, dropTargetProps} from './dropTarget';

const read = (rel: string) => readFileSync(new URL(rel, import.meta.url), 'utf8');

// Regression: after the move to Wails v3 the listing still used the v2 drop
// markers, the runtime ignored every drop, and drag-and-drop upload did nothing.
describe('native file drop wiring', () => {
    it('uses the names the installed Wails runtime looks for', () => {
        const runtime = read('../node_modules/@wailsio/runtime/dist/window.js');
        expect(runtime).toContain(`'${DROP_TARGET_ATTR}'`);
        expect(runtime).toContain(`'${DROP_TARGET_ACTIVE_CLASS}'`);
    });

    it('marks the element only when drops are wanted', () => {
        expect(dropTargetProps(true)).toHaveProperty(DROP_TARGET_ATTR);
        expect(dropTargetProps(false)).toEqual({});
    });

    it('is found from a child under the pointer, as the runtime does', () => {
        document.body.innerHTML = '<div id="area"><div id="row"><span id="cell"></span></div></div>';
        const area = document.getElementById('area')!;
        for (const [k, v] of Object.entries(dropTargetProps(true))) area.setAttribute(k, v);
        const hit = document.getElementById('cell')!.closest(`[${DROP_TARGET_ATTR}]`);
        expect(hit).toBe(area);
    });

    it('shows the overlay for the class the runtime adds', () => {
        const css = read('./styles/shell.css');
        expect(css).toContain(`.${DROP_TARGET_ACTIVE_CLASS} > .drop-overlay`);
        expect(css).not.toContain('wails-drop-target');
    });

    it('makes the listing area the drop target and keeps no v2 marker', () => {
        const src = read('./shell/Browser.tsx');
        expect(src).toMatch(/className="listing-area"\s+\{\.\.\.dropTargetProps\(/);
        expect(src).not.toContain('--wails-drop-target');
    });
});
