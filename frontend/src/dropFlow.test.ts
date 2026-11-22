import {describe, expect, it} from 'vitest';
import {decideDrop} from './dropFlow';

describe('decideDrop', () => {
    it('uploads without asking when nothing would be replaced', async () => {
        expect(await decideDrop(async () => [], '')).toEqual({action: 'upload'});
        expect(await decideDrop(async () => null as unknown as string[], '')).toEqual({action: 'upload'});
    });
    it('asks, naming them, when objects would be replaced', async () => {
        expect(await decideDrop(async () => ['a.bin', 'dir'], '')).toEqual({action: 'confirm', conflicts: ['a.bin', 'dir'], unchecked: false});
    });
    it('asks when the check fails, since it may overwrite', async () => {
        expect(await decideDrop(async () => { throw new Error('access denied'); }, '')).toEqual({action: 'confirm', conflicts: [], unchecked: true});
    });
    it('asks without checking when uploads are blocked, so the reason shows', async () => {
        let called = false;
        const d = await decideDrop(async () => { called = true; return []; }, 'Uploads are turned off');
        expect(d.action).toBe('confirm');
        expect(called).toBe(false);
    });
});
