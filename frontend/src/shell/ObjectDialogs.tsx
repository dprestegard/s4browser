import {FormEvent, useEffect, useRef, useState} from 'react';
import {CancelObjectWork, CreateFolder, DiscardDelete, PrepareDelete, PresignObject, RunDelete} from '../../bindings/s4browser/internal/app/app';
import {EventsOn} from '../wails';
import {Dialog} from '../ui/overlay';
import {Icon} from '../ui/Icon';
import {formatDate, formatRemaining, formatSize, plural} from '../ui/format';
import {DeletePreview, DeleteResult, ObjectProgress, Presigned, SelectionRequest} from '../types';

const errText = (err: unknown) => String(err).replace(/^Error: /, '');

// The same rules as objects.FolderKey in Go, so most mistakes show while typing.
export function folderNameProblem(name: string, prefix: string): string {
    if (name === '') return '';
    if (name.trim() !== name) return 'The name starts or ends with a space.';
    if (/[\\/]/.test(name)) return 'Leave out slashes: create one folder at a time.';
    if (name === '.' || name === '..') return `"${name}" is reserved.`;
    // eslint-disable-next-line no-control-regex
    if (/[\u0000-\u001f\u007f-\u009f]/.test(name)) return 'The name contains a control character.';
    if (new TextEncoder().encode(prefix + name + '/').length > 1024) return 'The key would be longer than S3 allows (1,024 bytes).';
    return '';
}

export function NewFolderDialog({profileId, bucket, prefix, onClose, onCreated}: {
    profileId: string;
    bucket: string;
    prefix: string;
    onClose: () => void;
    onCreated: (key: string) => void;
}) {
    const [name, setName] = useState('');
    const [error, setError] = useState('');
    const [busy, setBusy] = useState(false);
    const problem = folderNameProblem(name, prefix);

    const submit = async (e: FormEvent) => {
        e.preventDefault();
        if (!name || problem) return;
        setBusy(true);
        setError('');
        try {
            onCreated(await CreateFolder(profileId, bucket, prefix, name));
        } catch (err) {
            setError(errText(err));
            setBusy(false);
        }
    };

    return (
        <Dialog title="New folder" onClose={onClose} width={440}>
            <form onSubmit={submit} style={{display: 'flex', flexDirection: 'column', gap: 14}}>
                <div className="field">
                    <label htmlFor="nf-name">Name</label>
                    <input id="nf-name" className="input" data-autofocus autoComplete="off" spellCheck={false} value={name}
                           aria-invalid={!!problem} aria-describedby="nf-where"
                           onChange={(e) => { setName(e.target.value); setError(''); }}/>
                    <span id="nf-where" className="meta">In <span className="mono">{bucket}/{prefix}</span></span>
                </div>
                <p className="meta">S3 has no real folders. This adds an empty placeholder object whose key ends in "/", as the AWS console does.</p>
                {(problem || error) && <p className="meta t-danger" role="alert">{problem || error}</p>}
                <div className="actions">
                    <button type="button" className="btn" onClick={onClose}>Cancel</button>
                    <button type="submit" className="btn primary" disabled={!name || !!problem || busy}>{busy ? 'Creating…' : 'Create'}</button>
                </div>
            </form>
        </Dialog>
    );
}

// Deleting runs in steps: Go counts what the selection holds (listing each
// folder), the user confirms those numbers, then Go deletes exactly the keys it
// counted.
type DeleteStep =
    | {step: 'counting'; found: number}
    | {step: 'confirm'; preview: DeletePreview}
    | {step: 'deleting'; preview: DeletePreview; done: number}
    | {step: 'done'; preview: DeletePreview; result: DeleteResult}
    | {step: 'error'; message: string};

// Typing "delete" is asked for when this many objects or more would go.
const TYPE_TO_CONFIRM = 100;

export function DeleteDialog({profileId, bucket, sel, onClose, onDeleted}: {
    profileId: string;
    bucket: string;
    sel: SelectionRequest;
    onClose: () => void;
    onDeleted: () => void;
}) {
    const [s, setS] = useState<DeleteStep>({step: 'counting', found: 0});
    const [typed, setTyped] = useState('');
    const step = useRef(s);
    step.current = s;

    useEffect(() => {
        let live = true;
        // Progress events can arrive after the call they belong to has returned,
        // so they only ever update the step they were sent for.
        const off = EventsOn('s4:objects', (p: ObjectProgress) => setS((cur) => {
            if (p.phase === 'counting' && cur.step === 'counting') return {step: 'counting', found: p.done};
            if (p.phase === 'deleting' && cur.step === 'deleting') return {...cur, done: Math.max(cur.done, p.done)};
            return cur;
        }));
        PrepareDelete({profileId, bucket, listingId: sel.listingId, field: sel.field, desc: sel.desc, version: sel.version, ranges: sel.ranges} as never)
            .then((p) => {
                if (live) setS({step: 'confirm', preview: p as DeletePreview});
                else DiscardDelete((p as DeletePreview).id);
            })
            .catch((err) => live && setS({step: 'error', message: errText(err)}));
        return () => {
            live = false;
            off();
        };
        // The dialog is opened for one selection.
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, []);

    const close = () => {
        const cur = step.current;
        if (cur.step === 'counting') CancelObjectWork();
        if (cur.step === 'deleting') {
            CancelObjectWork(); // the result arrives and the dialog shows it
            return;
        }
        if (cur.step === 'confirm') DiscardDelete(cur.preview.id);
        onClose();
    };

    const run = async (preview: DeletePreview) => {
        setS({step: 'deleting', preview, done: 0});
        try {
            const result = await RunDelete(preview.id) as DeleteResult;
            setS({step: 'done', preview, result});
        } catch (err) {
            setS({step: 'error', message: errText(err)});
        }
        onDeleted();
    };

    const what = sel.folders
        ? plural(sel.count, 'selected item')
        : plural(sel.objects, 'object');

    if (s.step === 'counting' || s.step === 'error') {
        return (
            <Dialog title={`Delete ${what}?`} onClose={close} width={520} role="alertdialog">
                {s.step === 'counting' ? (
                    <p className="dock-line" role="status">
                        <span className="spinner" aria-hidden="true"/>
                        {sel.folders ? `Counting what the selected folders hold… ${plural(s.found, 'object')} so far.` : 'Checking the selection…'} Nothing is deleted yet.
                    </p>
                ) : (
                    <p className="t-danger" role="alert">{s.message}</p>
                )}
                <div className="actions">
                    <button type="button" className="btn" data-autofocus onClick={close}>{s.step === 'counting' ? 'Cancel' : 'Close'}</button>
                </div>
            </Dialog>
        );
    }

    const p = s.preview;
    if (s.step === 'confirm') {
        const needType = p.objects >= TYPE_TO_CONFIRM;
        const sample = p.sample ?? [];
        return (
            <Dialog title={`Delete ${plural(p.objects, 'object')}?`} onClose={close} width={560} role="alertdialog">
                <p>
                    {plural(p.objects, 'object')} ({formatSize(p.bytes)}) in <span className="mono" style={{color: 'var(--text)'}}>{p.bucket}/{p.prefix}</span>
                    {p.folders > 0 ? `, including everything inside ${plural(p.folders, 'selected folder')}` : ''}.
                    {p.placeholders > 0 ? ` ${plural(p.placeholders, 'is a folder placeholder', 'are folder placeholders')}.` : ''}
                </p>
                <ul className="delete-sample mono" aria-label="First keys">
                    {sample.map((k) => <li key={k} className="ellipsis" title={k}>{k.startsWith(p.prefix) ? k.slice(p.prefix.length) : k}</li>)}
                    {p.objects > sample.length && <li className="meta">…and {(p.objects - sample.length).toLocaleString('en-US')} more</li>}
                </ul>
                <VersioningNote preview={p}/>
                {p.transfers > 0 && (
                    <div className="banner warn"><Icon name="alert"/>
                        <div>{plural(p.transfers, 'transfer')} on these objects {p.transfers === 1 ? 'is' : 'are'} not finished. An upload that completes after the delete puts its object back.</div>
                    </div>
                )}
                {needType && (
                    <div className="field">
                        <label htmlFor="del-confirm">Type <span className="mono">delete</span> to confirm</label>
                        <input id="del-confirm" className="input" autoComplete="off" spellCheck={false} value={typed} onChange={(e) => setTyped(e.target.value)}
                               onKeyDown={(e) => e.key === 'Enter' && typed.trim().toLowerCase() === 'delete' && run(p)}/>
                    </div>
                )}
                <div className="actions">
                    <button type="button" className="btn" data-autofocus={needType ? undefined : true} onClick={close}>Cancel</button>
                    <button type="button" className="btn danger" disabled={needType && typed.trim().toLowerCase() !== 'delete'} onClick={() => run(p)}>
                        <Icon name="trash"/>Delete {plural(p.objects, 'object')}
                    </button>
                </div>
            </Dialog>
        );
    }

    if (s.step === 'deleting') {
        const pct = p.objects ? Math.round((s.done / p.objects) * 100) : 0;
        return (
            <Dialog title={`Deleting ${plural(p.objects, 'object')}`} onClose={close} width={520} role="alertdialog">
                <div className="qprogress" role="status">
                    <div className="bar" aria-hidden="true"><span style={{width: `${pct}%`}}/></div>
                    <span className="meta">{s.done.toLocaleString('en-US')} of {p.objects.toLocaleString('en-US')}</span>
                </div>
                <p className="meta">Stopping keeps what S3 has already deleted. It cannot be put back.</p>
                <div className="actions">
                    <button type="button" className="btn" data-autofocus onClick={close}>Stop</button>
                </div>
            </Dialog>
        );
    }

    const r = s.result;
    const failed = r.failed ?? [];
    const clean = r.failedTotal === 0 && r.notTried === 0 && !r.stopped;
    return (
        <Dialog title={clean ? `Deleted ${plural(r.deleted, 'object')}` : 'The delete did not finish'} onClose={onClose} width={560}>
            <p>
                {plural(r.deleted, 'object')} deleted
                {r.markers > 0 ? ` (S3 added ${plural(r.markers, 'delete marker')}; older versions stay)` : ''}.
                {r.failedTotal > 0 ? ` ${plural(r.failedTotal, 'object')} S3 refused.` : ''}
                {r.notTried > 0 ? ` ${plural(r.notTried, 'object')} not deleted, because the delete stopped.` : ''}
            </p>
            {r.stopped && <p className="meta t-warn" role="alert">{r.stopped === 'Cancelled.' ? 'You stopped the delete.' : `S3 failed a request: ${r.stopped}`}</p>}
            {failed.length > 0 && (
                <div className="refusals" role="table" aria-label="Objects not deleted">
                    {failed.map((f) => (
                        <div key={f.key} className="refusal" role="row">
                            <span className="mono ellipsis" role="cell" title={f.key}>{f.key}</span>
                            <span role="cell">{f.code ? `${f.code}: ` : ''}{f.message}</span>
                        </div>
                    ))}
                    {r.failedTotal > failed.length && <p className="meta">…and {(r.failedTotal - failed.length).toLocaleString('en-US')} more</p>}
                </div>
            )}
            <div className="actions">
                <button type="button" className="btn primary" data-autofocus onClick={onClose}>Close</button>
            </div>
        </Dialog>
    );
}

function VersioningNote({preview: p}: {preview: DeletePreview}) {
    switch (p.versioning) {
        case 'enabled':
            return (
                <div className="banner warn"><Icon name="alert"/>
                    <div><strong>Versioning is on for this bucket</strong>
                        S3 hides each object behind a delete marker. Earlier versions stay, keep costing storage, and can be restored in the AWS console.</div>
                </div>
            );
        case 'suspended':
            return (
                <div className="banner warn"><Icon name="alert"/>
                    <div><strong>Versioning is suspended for this bucket</strong>
                        Objects with older versions get a delete marker and those versions stay. Other objects are deleted for good.</div>
                </div>
            );
        case 'unknown':
            return (
                <div className="banner danger"><Icon name="alert"/>
                    <div><strong>This probably cannot be undone</strong>
                        Whether the bucket keeps versions could not be checked{p.versioningError ? ` (${p.versioningError.replace(/\.$/, '')})` : ''}.</div>
                </div>
            );
    }
    return (
        <div className="banner danger"><Icon name="alert"/>
            <div><strong>This cannot be undone</strong>Versioning is off for this bucket, so S3 keeps no copy.</div>
        </div>
    );
}

const durations: [string, number][] = [
    ['15 minutes', 15 * 60], ['1 hour', 3600], ['12 hours', 12 * 3600], ['1 day', 86400], ['7 days (the most S3 allows)', 7 * 86400],
];

export function PresignDialog({profileId, bucket, objectKey, onClose}: {
    profileId: string;
    bucket: string;
    objectKey: string;
    onClose: () => void;
}) {
    const [seconds, setSeconds] = useState(3600);
    const [link, setLink] = useState<Presigned | null>(null);
    const [error, setError] = useState('');
    const [busy, setBusy] = useState(false);
    const [copied, setCopied] = useState(false);
    const [, tick] = useState(0);
    const urlRef = useRef<HTMLTextAreaElement>(null);

    useEffect(() => {
        if (!link) return;
        const t = window.setInterval(() => tick((n) => n + 1), 30_000);
        return () => window.clearInterval(t);
    }, [link]);

    const create = async () => {
        setBusy(true);
        setError('');
        setCopied(false);
        try {
            setLink(await PresignObject(profileId, bucket, objectKey, seconds) as Presigned);
        } catch (err) {
            setError(errText(err));
        } finally {
            setBusy(false);
        }
    };

    const copy = () => {
        if (!link) return;
        urlRef.current?.select();
        navigator.clipboard?.writeText(link.url).then(() => setCopied(true), () => setCopied(false));
    };

    const left = link ? new Date(link.expires).getTime() - Date.now() : 0;
    const name = objectKey.split('/').pop() || objectKey;
    return (
        <Dialog title="Share a download link" onClose={onClose} width={560}>
            <p>
                For <span className="mono" style={{color: 'var(--text)'}} title={`${bucket}/${objectKey}`}>{name}</span>.
                Anyone with the link can download the object until it expires. A link cannot be withdrawn early.
            </p>
            <div className="field">
                <label htmlFor="ps-life">Expires after</label>
                <div style={{display: 'flex', gap: 8}}>
                    <select id="ps-life" className="select" style={{flex: 1, minWidth: 0}} value={seconds}
                            onChange={(e) => { setSeconds(Number(e.target.value)); setLink(null); }}>
                        {durations.map(([label, s]) => <option key={s} value={s}>{label}</option>)}
                    </select>
                    <button type="button" className="btn primary" data-autofocus disabled={busy} onClick={create}>
                        <Icon name="link"/>{busy ? 'Signing…' : link ? 'Create again' : 'Create link'}
                    </button>
                </div>
            </div>
            {error && <p className="meta t-danger" role="alert">{error}</p>}
            {link && (
                <>
                    <div className="field">
                        <label htmlFor="ps-url">Link</label>
                        <textarea id="ps-url" ref={urlRef} className="input mono presign-url" readOnly rows={4} value={link.url}
                                  onFocus={(e) => e.currentTarget.select()}/>
                    </div>
                    <p className="meta" role="status">
                        {left > 0 ? <>Works until <strong>{formatDate(link.expires, true)}</strong>, {formatRemaining(left)} from now.</> : <span className="t-danger">This link has expired.</span>}
                    </p>
                    {link.limitedBy === 'credentials' && (
                        <div className="banner warn"><Icon name="clock"/>
                            <div><strong>Shorter than you asked</strong>
                                The profile's credentials expire at {formatDate(link.credentialsExpire, true)}, and S3 refuses the link from then on. Sign in again for a longer one.</div>
                        </div>
                    )}
                </>
            )}
            <div className="actions">
                <button type="button" className="btn" onClick={onClose}>Close</button>
                <button type="button" className="btn primary" disabled={!link || left <= 0} onClick={copy}>
                    <Icon name={copied ? 'check' : 'copy'}/>{copied ? 'Copied' : 'Copy link'}
                </button>
            </div>
        </Dialog>
    );
}
