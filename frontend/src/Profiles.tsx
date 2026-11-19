import {FormEvent, useCallback, useEffect, useState} from 'react';
import {
    AddKeyProfile,
    CheckProfile,
    DeleteProfile,
    Profiles as LoadProfiles,
    SelectProfile,
} from '../wailsjs/go/main/App';

// Stage 1.1 working panel. Stage 1.3 replaces it with the designed app shell.
// Secrets pass through this page only when a new key pair is typed, and Go never
// sends one back.

type Profile = {
    id: string;
    name: string;
    source: string;
    kind: string;
    region?: string;
    keyHint?: string;
    supported: boolean;
    problem?: string;
};
type State = {profiles: Profile[]; activeId: string; warnings?: string[]; error?: string};

const kindLabels: Record<string, string> = {
    'vault-keys': 'Keys in vault',
    static: 'Static keys',
    session: 'Session keys',
    'credential-process': 'credential_process',
    other: 'Unsupported',
    none: 'No credentials',
};

const emptyForm = {name: '', accessKey: '', secret: '', region: ''};

export function Profiles({onActive}: {onActive?: (id: string) => void}) {
    const [state, setState] = useState<State>({profiles: [], activeId: ''});
    const [form, setForm] = useState(emptyForm);
    const [message, setMessage] = useState('');
    const [busy, setBusy] = useState('');

    const reload = useCallback(() => LoadProfiles().then((s) => {
        setState(s as State);
        onActive?.((s as State).activeId);
    }), [onActive]);
    useEffect(() => {
        reload();
    }, [reload]);

    const run = async (action: () => Promise<unknown>) => {
        setMessage('');
        try {
            await action();
        } catch (err) {
            setMessage(String(err));
        }
        await reload();
    };

    const check = async (p: Profile) => {
        setBusy(p.id);
        setMessage('');
        const r = await CheckProfile(p.id);
        setBusy('');
        if (!r.ok) {
            setMessage(`${p.name}: ${r.message}`);
            return;
        }
        const expires = r.expires ? `, expires ${new Date(r.expires as unknown as string).toLocaleString()}` : '';
        setMessage(`${p.name}: credentials loaded (${r.keyHint}${expires}). AWS has not been contacted.`);
    };

    const add = (e: FormEvent) => {
        e.preventDefault();
        run(async () => {
            await AddKeyProfile(form.name, form.accessKey, form.secret, form.region);
            setForm(emptyForm);
        });
    };

    const field = (key: keyof typeof emptyForm) => ({
        value: form[key],
        onChange: (e: {target: {value: string}}) => setForm({...form, [key]: e.target.value}),
    });

    return (
        <section className="profiles">
            <h2>Profiles</h2>
            {state.error && <p className="problem">{state.error}</p>}
            {state.warnings?.map((w) => <p className="problem" key={w}>{w}</p>)}
            {state.profiles.length === 0 && !state.error && <p className="meta">No profiles found.</p>}
            <ul>
                {state.profiles.map((p) => (
                    <li key={p.id} className="profile">
                        <label>
                            <input
                                type="radio"
                                name="active-profile"
                                checked={state.activeId === p.id}
                                onChange={() => run(() => SelectProfile(p.id))}
                            />
                            <strong>{p.name}</strong>
                        </label>
                        <span className="meta">
                            {kindLabels[p.kind] ?? p.kind}
                            {p.region ? `, ${p.region}` : ''}
                            {p.keyHint ? `, ${p.keyHint}` : ''}
                        </span>
                        <button type="button" disabled={busy === p.id} onClick={() => check(p)}>
                            {busy === p.id ? 'Loading...' : 'Load credentials'}
                        </button>
                        {p.source === 'app' && (
                            <button type="button" onClick={() => run(() => DeleteProfile(p.id))}>Delete</button>
                        )}
                        {p.problem && <div className="problem">{p.problem}</div>}
                    </li>
                ))}
            </ul>
            {message && <p role="status" className="status">{message}</p>}
            <form onSubmit={add} autoComplete="off">
                <h3>Add access keys</h3>
                <input aria-label="Profile name" placeholder="Profile name" required {...field('name')} />
                <input aria-label="Access key ID" placeholder="Access key ID" required spellCheck={false} {...field('accessKey')} />
                <input aria-label="Secret access key" placeholder="Secret access key" type="password" required {...field('secret')} />
                <input aria-label="Default region" placeholder="Default region (optional)" {...field('region')} />
                <button type="submit">Save to credential vault</button>
            </form>
        </section>
    );
}
