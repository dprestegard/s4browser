import {useEffect, useState} from 'react';
import {ObjectMetadata as FetchMetadata, ObjectTags, SaveObjectTags} from '../../bindings/s4browser/internal/app/app';
import {Icon} from '../ui/Icon';
import {ObjectMetadata, Tag} from '../types';

const errText = (err: unknown) => String(err).replace(/^Error: /, '');

// Waits this long after the selection settles before asking S3, so arrowing
// through rows does not send a HEAD for each one.
const SETTLE_MS = 250;

// The metadata, tags and share button for one selected object. It is keyed by
// the object, so selecting another one starts afresh.
export function ObjectExtras({profileId, bucket, objectKey, onShare}: {
    profileId: string;
    bucket: string;
    objectKey: string;
    onShare: () => void;
}) {
    const [meta, setMeta] = useState<ObjectMetadata | null>(null);
    const [metaErr, setMetaErr] = useState('');
    const [tags, setTags] = useState<Tag[] | null>(null);
    const [tagErr, setTagErr] = useState('');
    const [reload, setReload] = useState(0);

    useEffect(() => {
        let live = true;
        setMetaErr('');
        setTagErr('');
        const t = window.setTimeout(() => {
            FetchMetadata(profileId, bucket, objectKey)
                .then((m) => live && setMeta(m as ObjectMetadata))
                .catch((err) => live && setMetaErr(errText(err)));
            ObjectTags(profileId, bucket, objectKey)
                .then((t) => live && setTags((t ?? []) as Tag[]))
                .catch((err) => live && setTagErr(errText(err)));
        }, SETTLE_MS);
        return () => {
            live = false;
            window.clearTimeout(t);
        };
    }, [profileId, bucket, objectKey, reload]);

    return (
        <>
            <button type="button" className="btn" onClick={onShare} style={{alignSelf: 'flex-start'}}>
                <Icon name="link"/>Share link…
            </button>

            <section className="stack" aria-label="Metadata">
                <span className="label">Metadata</span>
                {metaErr ? <p className="meta t-danger">{metaErr} <button type="button" className="btn ghost small" onClick={() => setReload((n) => n + 1)}>Try again</button></p>
                    : !meta ? <p className="meta">Loading…</p>
                        : <MetadataList m={meta}/>}
            </section>

            <section className="stack" aria-label="Tags">
                {tagErr ? (
                    <>
                        <span className="label">Tags</span>
                        <p className="meta t-danger">{tagErr} <button type="button" className="btn ghost small" onClick={() => setReload((n) => n + 1)}>Try again</button></p>
                    </>
                ) : !tags ? (
                    <><span className="label">Tags</span><p className="meta">Loading…</p></>
                ) : (
                    <TagEditor tags={tags} onSave={async (after) => {
                        const saved = await SaveObjectTags(profileId, bucket, objectKey, tags, after);
                        setTags((saved ?? []) as Tag[]);
                    }} onReload={() => { setTags(null); setReload((n) => n + 1); }}/>
                )}
            </section>
        </>
    );
}

function MetadataList({m}: {m: ObjectMetadata}) {
    const user = Object.entries(m.metadata ?? {}).sort(([a], [b]) => a.localeCompare(b));
    const row = (label: string, value: string | undefined, mono = false) =>
        value ? <><dt>{label}</dt><dd className={mono ? 'mono' : undefined}>{value}</dd></> : null;
    return (
        <dl className="kv">
            {row('Content type', m.contentType)}
            {row('ETag', m.etag, true)}
            {m.parts ? row('Uploaded as', `${m.parts.toLocaleString('en-US')} parts`) : row('Uploaded as', 'one request')}
            {row('Checksum', m.checksum, true)}
            {row('Encryption', m.encryption ? m.encryption + (m.kmsKeyId ? ` (${m.kmsKeyId})` : '') : 'none reported')}
            {row('Version', m.versionId, true)}
            {row('Cache control', m.cacheControl)}
            {row('Disposition', m.contentDisposition)}
            {row('Encoding', m.contentEncoding)}
            {row('Language', m.contentLanguage)}
            {row('Expires', m.expires)}
            {row('Restore', m.restore)}
            {row('Object lock', m.objectLock)}
            {user.map(([k, v]) => (
                <span key={k} style={{display: 'contents'}}><dt className="mono" title={`x-amz-meta-${k}`}>{k}</dt><dd>{v}</dd></span>
            ))}
            {user.length === 0 && <><dt>User metadata</dt><dd className="meta">none</dd></>}
        </dl>
    );
}

const MAX_TAGS = 10;
const tagChars = /^[\p{L}\p{N} +\-=._:/@]*$/u;

// The same rules as objects.ValidateTags in Go.
export function tagProblem(tags: Tag[]): string {
    if (tags.length > MAX_TAGS) return `An object can have at most ${MAX_TAGS} tags.`;
    const seen = new Set<string>();
    for (const t of tags) {
        const key = [...t.key].length;
        if (key === 0) return 'Every tag needs a key.';
        if (key > 128) return `Tag key "${t.key.slice(0, 24)}…" is longer than 128 characters.`;
        if ([...t.value].length > 256) return `The value of "${t.key}" is longer than 256 characters.`;
        if (t.key.toLowerCase().startsWith('aws:')) return `Keys starting with "aws:" are reserved.`;
        if (!tagChars.test(t.key)) return `Tag key "${t.key}" may use only letters, digits, spaces and + - = . _ : / @.`;
        if (!tagChars.test(t.value)) return `The value of "${t.key}" may use only letters, digits, spaces and + - = . _ : / @.`;
        if (seen.has(t.key)) return `Tag key "${t.key}" is used twice.`;
        seen.add(t.key);
    }
    return '';
}

function TagEditor({tags, onSave, onReload}: {tags: Tag[]; onSave: (after: Tag[]) => Promise<void>; onReload: () => void}) {
    const [draft, setDraft] = useState<Tag[] | null>(null);
    const [error, setError] = useState('');
    const [busy, setBusy] = useState(false);

    if (!draft) {
        return (
            <>
                <div style={{display: 'flex', alignItems: 'center', gap: 8}}>
                    <span className="label" style={{flex: 1}}>Tags</span>
                    <button type="button" className="btn ghost small" onClick={() => { setDraft(tags.map((t) => ({...t}))); setError(''); }}>
                        Edit
                    </button>
                </div>
                {tags.length === 0 ? <p className="meta">No tags.</p> : (
                    <dl className="kv">
                        {tags.map((t) => <span key={t.key} style={{display: 'contents'}}><dt>{t.key}</dt><dd>{t.value || <span className="meta">empty</span>}</dd></span>)}
                    </dl>
                )}
            </>
        );
    }

    const problem = tagProblem(draft);
    const set = (i: number, part: Partial<Tag>) => setDraft(draft.map((t, j) => (j === i ? {...t, ...part} : t)));
    const save = async () => {
        if (problem) return;
        setBusy(true);
        setError('');
        try {
            await onSave(draft);
            setDraft(null);
        } catch (err) {
            setError(errText(err));
        } finally {
            setBusy(false);
        }
    };
    const changed = errorIsStale(error);

    return (
        <form className="stack tag-edit" onSubmit={(e) => { e.preventDefault(); void save(); }}>
            <span className="label">Tags</span>
            {draft.map((t, i) => (
                <div key={i} className="tag-row">
                    <input className="input" aria-label={`Tag ${i + 1} key`} placeholder="Key" value={t.key} spellCheck={false}
                           autoFocus={i === draft.length - 1 && t.key === ''} onChange={(e) => set(i, {key: e.target.value})}/>
                    <input className="input" aria-label={`Tag ${i + 1} value`} placeholder="Value" value={t.value} spellCheck={false}
                           onChange={(e) => set(i, {value: e.target.value})}/>
                    <button type="button" className="btn ghost icon-only" aria-label={`Remove tag ${t.key || i + 1}`} title="Remove"
                            onClick={() => setDraft(draft.filter((_, j) => j !== i))}>
                        <Icon name="close" size="sm"/>
                    </button>
                </div>
            ))}
            <button type="button" className="btn ghost small" style={{alignSelf: 'flex-start'}} disabled={draft.length >= MAX_TAGS}
                    onClick={() => setDraft([...draft, {key: '', value: ''}])}>
                <Icon name="plus" size="sm"/>Add tag{draft.length >= MAX_TAGS ? ' (10 at most)' : ''}
            </button>
            {(problem || error) && <p className="meta t-danger" role="alert">{problem || error}</p>}
            <p className="meta">Saving replaces all of the object's tags. It does not copy or rewrite the object.</p>
            <div style={{display: 'flex', gap: 8, justifyContent: 'flex-end'}}>
                <button type="button" className="btn small" onClick={() => { setDraft(null); setError(''); }}>Cancel</button>
                {changed
                    ? <button type="button" className="btn small primary" onClick={onReload}>Reload tags</button>
                    : <button type="submit" className="btn small primary" disabled={!!problem || busy}>{busy ? 'Saving…' : 'Save'}</button>}
            </div>
        </form>
    );
}

const errorIsStale = (e: string) => e.includes('changed since they were loaded');
