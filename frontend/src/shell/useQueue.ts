import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import {QueueJobs, SetTransferSettings, TransferSettings as FetchSettings, TransferStatus} from '../../wailsjs/go/app/App';
import {EventsOn} from '../../wailsjs/runtime/runtime';
import {JobView, QueueEvent, QueueStatus, SettingsState, TransferSettings} from '../types';

// The transfer queue as the UI sees it. Go owns the jobs; the page keeps one
// JobView per job, merged from the batched "s4:queue" events (about 5 a second,
// changed jobs only). A gap in the sequence numbers, or a resync flag, reloads
// the whole list with QueueJobs.

export type QueueTab = 'progress' | 'failed' | 'done';

export function tabOf(j: JobView): QueueTab {
    if (!j.finished) return 'progress';
    return j.state === 'failed' ? 'failed' : 'done';
}

export type Queue = {
    status: QueueStatus | null;
    /** Error from loading the list or the status, if any. */
    error: string;
    /** Jobs of each tab: unfinished in submission order, finished newest first.
     * Rebuilt when anything changes. */
    lists: Record<QueueTab, JobView[]>;
    /** Totals over the unfinished jobs. */
    totals: {running: number; waiting: number; paused: number; bytesPerSec: number; leftBytes: number; anyPause: boolean; anyResume: boolean};
    settings: SettingsState | null;
    saveSettings: (s: TransferSettings) => Promise<SettingsState>;
    /** Reloads the list: after a submit, so new jobs show at once. */
    reload: () => void;
    /** Registers a function called with every job whose state changed. initial
     * is true for the jobs of the first load, which were not seen changing. */
    onChange: (fn: (j: JobView, before: JobView | undefined, initial: boolean) => void) => () => void;
};

const unavailable: QueueStatus = {available: false, uploadsDisabled: false, warnings: [], cleanup: {running: false, finished: false, uploadsAborted: 0, tempsRemoved: 0, pending: 0}};

export function useQueue(): Queue {
    const jobs = useRef(new Map<string, JobView>());
    const order = useRef<string[]>([]);
    const [version, setVersion] = useState(0);
    const [status, setStatus] = useState<QueueStatus | null>(null);
    const [error, setError] = useState('');
    const [settings, setSettings] = useState<SettingsState | null>(null);
    const lastSeq = useRef(0);
    const loading = useRef(false);
    const pending = useRef<QueueEvent[]>([]);
    const primed = useRef(false);
    const listeners = useRef(new Set<(j: JobView, before: JobView | undefined, initial: boolean) => void>());

    const bump = () => setVersion((v) => v + 1);

    const put = (j: JobView) => {
        const before = jobs.current.get(j.id);
        if (!before) order.current.push(j.id);
        jobs.current.set(j.id, j);
        if (before?.state !== j.state) listeners.current.forEach((fn) => fn(j, before, !primed.current));
    };

    const apply = (ev: QueueEvent) => {
        for (const j of ev.jobs ?? []) put(j);
        if (ev.removed?.length) {
            const gone = new Set(ev.removed);
            ev.removed.forEach((id) => jobs.current.delete(id));
            order.current = order.current.filter((id) => !gone.has(id));
        }
    };

    const reload = useCallback(() => {
        if (loading.current) return;
        loading.current = true;
        QueueJobs()
            .then((list) => {
                const fresh = (list ?? []) as JobView[];
                const keep = new Set(fresh.map((j) => j.id));
                for (const id of order.current) if (!keep.has(id)) jobs.current.delete(id);
                order.current = order.current.filter((id) => keep.has(id));
                fresh.forEach(put);
                // Events that arrived while loading are newer than or as new as the list.
                pending.current.splice(0).forEach(apply);
                primed.current = true;
                setError('');
            })
            .catch((err) => setError(String(err)))
            .finally(() => {
                loading.current = false;
                bump();
            });
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);

    useEffect(() => {
        TransferStatus().then((s) => setStatus(s as QueueStatus)).catch((err) => {
            setStatus(unavailable);
            setError(String(err));
        });
        FetchSettings().then((s) => setSettings(s as SettingsState)).catch(() => {});
        reload();
        const offQueue = EventsOn('s4:queue', (ev: QueueEvent) => {
            const gap = lastSeq.current !== 0 && ev.seq !== lastSeq.current + 1;
            lastSeq.current = ev.seq;
            if (ev.resync || gap) {
                reload();
                return;
            }
            if (loading.current) {
                pending.current.push(ev);
                return;
            }
            apply(ev);
            bump();
        });
        const offStatus = EventsOn('s4:queue-status', (s: QueueStatus) => setStatus(s));
        return () => {
            offQueue();
            offStatus();
        };
    }, [reload]);

    const lists = useMemo(() => {
        const out: Record<QueueTab, JobView[]> = {progress: [], failed: [], done: []};
        for (const id of order.current) {
            const j = jobs.current.get(id);
            if (j) out[tabOf(j)].push(j);
        }
        // Finished jobs: the most recently submitted first.
        out.failed.reverse();
        out.done.reverse();
        return out;
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [version]);

    const totals = useMemo(() => {
        const t = {running: 0, waiting: 0, paused: 0, bytesPerSec: 0, leftBytes: 0, anyPause: false, anyResume: false};
        for (const j of lists.progress) {
            if (j.state === 'running') t.running++;
            else if (j.state === 'paused' || j.state === 'pausing') t.paused++;
            else t.waiting++;
            t.bytesPerSec += j.bytesPerSec;
            t.leftBytes += Math.max(0, j.totalBytes - j.doneBytes);
            t.anyPause ||= j.canPause;
            t.anyResume ||= j.canResume;
        }
        return t;
    }, [lists]);

    const saveSettings = useCallback(async (s: TransferSettings) => {
        const next = (await SetTransferSettings(s as never)) as SettingsState;
        setSettings(next);
        return next;
    }, []);

    const onChange = useCallback((fn: (j: JobView, before: JobView | undefined, initial: boolean) => void) => {
        listeners.current.add(fn);
        return () => {
            listeners.current.delete(fn);
        };
    }, []);

    return {status, error, lists, totals, settings, saveSettings, reload, onChange};
}
