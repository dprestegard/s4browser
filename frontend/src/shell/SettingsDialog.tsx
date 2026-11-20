import {FormEvent, useState} from 'react';
import {Dialog} from '../ui/overlay';
import {formatSize} from '../ui/format';
import {SettingsState, TransferSettings} from '../types';

// Transfer settings. Go validates and saves them and applies them while
// transfers run: concurrency at the next part, caps at once, part size to files
// that have not started.

const MiB = 1 << 20;

// "4 files · 16 parts each · 64 MiB parts", the dock button's label.
export function settingsSummary(s: TransferSettings): string {
    return `${s.parallelFiles} files · ${s.partsPerFile} parts each · ${formatSize(s.partSize || 64 * MiB).replace('.0 ', ' ')} parts`;
}

// "↑ 50.0 MiB/s · ↓ no cap", for the top bar.
export function capSummary(s: TransferSettings | undefined): string {
    if (!s || (!s.uploadBytesPerSec && !s.downloadBytesPerSec)) return 'Bandwidth: no cap';
    const one = (n: number) => (n ? formatCap(n) : 'no cap');
    return `Bandwidth: ↑ ${one(s.uploadBytesPerSec)} · ↓ ${one(s.downloadBytesPerSec)}`;
}

function formatCap(bytesPerSec: number): string {
    const mbit = (bytesPerSec * 8) / 1e6;
    return mbit >= 1000 ? `${+(mbit / 1000).toFixed(2)} Gbit/s` : `${+mbit.toFixed(1)} Mbit/s`;
}

type CapUnit = 'none' | 'mbit' | 'mib';
type Cap = {unit: CapUnit; value: string};

function capFromBytes(n: number): Cap {
    if (!n) return {unit: 'none', value: ''};
    const mbit = (n * 8) / 1e6;
    if (Number.isInteger(+mbit.toFixed(3))) return {unit: 'mbit', value: String(+mbit.toFixed(3))};
    return {unit: 'mib', value: String(+(n / MiB).toFixed(2))};
}

function capToBytes(c: Cap): number | null {
    if (c.unit === 'none') return 0;
    const v = Number(c.value);
    if (!Number.isFinite(v) || v <= 0) return null;
    return Math.round(c.unit === 'mbit' ? (v * 1e6) / 8 : v * MiB);
}

export function SettingsDialog({state, save, onClose}: {
    state: SettingsState;
    save: (s: TransferSettings) => Promise<unknown>;
    onClose: () => void;
}) {
    const {limits, defaults} = state;
    const [form, setForm] = useState(() => toForm(state.current));
    const [error, setError] = useState('');
    const [busy, setBusy] = useState(false);

    // Part sizes on offer: powers of two between the limits, plus the current one.
    const sizes: number[] = [];
    for (let s = 8 * MiB; s <= limits.maxPartSize; s *= 2) if (s >= limits.minPartSize) sizes.push(s);
    const current = Number(form.partSize);
    if (current && !sizes.includes(current)) sizes.push(current);
    sizes.sort((a, b) => a - b);

    const submit = async (e: FormEvent) => {
        e.preventDefault();
        const up = capToBytes(form.up);
        const down = capToBytes(form.down);
        if (up === null || down === null) {
            setError('A bandwidth cap must be a number above zero, or "No cap".');
            return;
        }
        setBusy(true);
        setError('');
        try {
            await save({
                parallelFiles: Number(form.parallelFiles),
                partsPerFile: Number(form.partsPerFile),
                totalParts: Number(form.totalParts),
                partSize: Number(form.partSize),
                uploadBytesPerSec: up,
                downloadBytesPerSec: down,
            });
            onClose();
        } catch (err) {
            setError(String(err));
        } finally {
            setBusy(false);
        }
    };

    const num = (key: 'parallelFiles' | 'partsPerFile' | 'totalParts', label: string, hint: string, min: number, max: number) => (
        <div className="field">
            <label htmlFor={`ts-${key}`}>{label} <span className="hint">{min} to {max}, default {defaults[key]}</span></label>
            <input id={`ts-${key}`} className="input" type="number" min={min} max={max} step={1} required
                   value={form[key]} onChange={(e) => setForm({...form, [key]: e.target.value})}/>
            <span className="meta">{hint}</span>
        </div>
    );

    const cap = (key: 'up' | 'down', label: string) => (
        <div className="field">
            <label htmlFor={`ts-${key}`}>{label}</label>
            <div style={{display: 'flex', gap: 8}}>
                <select className="select" aria-label={`${label} unit`} value={form[key].unit}
                        onChange={(e) => setForm({...form, [key]: {unit: e.target.value as CapUnit, value: form[key].value}})}>
                    <option value="none">No cap</option>
                    <option value="mbit">Mbit/s</option>
                    <option value="mib">MiB/s</option>
                </select>
                <input id={`ts-${key}`} className="input" type="number" min={0} step="any" style={{flex: 1, minWidth: 0}}
                       disabled={form[key].unit === 'none'} required={form[key].unit !== 'none'}
                       value={form[key].unit === 'none' ? '' : form[key].value}
                       placeholder={form[key].unit === 'none' ? 'Unlimited' : ''}
                       onChange={(e) => setForm({...form, [key]: {...form[key], value: e.target.value}})}/>
            </div>
        </div>
    );

    return (
        <Dialog title="Transfer settings" onClose={onClose} width={520}>
            <form onSubmit={submit} style={{display: 'flex', flexDirection: 'column', gap: 14}}>
                <div style={{display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) minmax(0, 1fr)', gap: 14}}>
                    {num('parallelFiles', 'Files at once', 'Files transferring at the same time.', limits.minParallelFiles, limits.maxParallelFiles)}
                    {num('partsPerFile', 'Parts per file', 'Connections one file may use.', limits.minPartsPerFile, limits.maxPartsPerFile)}
                    {num('totalParts', 'Parts in total', 'Connections across all files.', limits.minTotalParts, limits.maxTotalParts)}
                    <div className="field">
                        <label htmlFor="ts-part">Part size <span className="hint">default {formatSize(defaults.partSize || 64 * MiB)}</span></label>
                        <select id="ts-part" className="select" value={form.partSize}
                                onChange={(e) => setForm({...form, partSize: e.target.value})}>
                            {sizes.map((s) => <option key={s} value={String(s)}>{formatSize(s)}</option>)}
                        </select>
                        <span className="meta">Very large files use bigger parts automatically, as S3 allows 10,000.</span>
                    </div>
                    {cap('up', 'Upload cap')}
                    {cap('down', 'Download cap')}
                </div>
                <p className="meta">Changes apply to running transfers: connection counts as parts start, caps at once, part size to files that have not started yet.</p>
                {error && <p className="meta t-danger" role="alert">{error}</p>}
                <div className="actions">
                    <button type="button" className="btn ghost" style={{marginRight: 'auto'}}
                            onClick={() => setForm(toForm(defaults))}>Reset to defaults</button>
                    <button type="button" className="btn" onClick={onClose}>Cancel</button>
                    <button type="submit" className="btn primary" disabled={busy}>{busy ? 'Saving…' : 'Apply'}</button>
                </div>
            </form>
        </Dialog>
    );
}

function toForm(s: TransferSettings) {
    return {
        parallelFiles: String(s.parallelFiles),
        partsPerFile: String(s.partsPerFile),
        totalParts: String(s.totalParts),
        partSize: String(s.partSize || 64 * MiB),
        up: capFromBytes(s.uploadBytesPerSec),
        down: capFromBytes(s.downloadBytesPerSec),
    };
}
