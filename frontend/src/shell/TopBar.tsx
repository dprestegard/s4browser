import {FormEvent, useEffect, useRef, useState} from 'react';
import {Icon} from '../ui/Icon';
import {Dialog, useDismiss} from '../ui/overlay';
import {formatRemaining} from '../ui/format';
import {osName, vaultName} from '../ui/platform';
import {Licenses} from '../../wailsjs/go/main/App';
import {Density, ThemeSetting} from '../settings';
import {kindLabels} from '../types';
import {Profiles} from './useProfiles';

export function TopBar({profiles, theme, setTheme, density, setDensity, detailsOpen, toggleDetails, bandwidth}: {
    profiles: Profiles;
    theme: ThemeSetting;
    setTheme: (t: ThemeSetting) => void;
    density: Density;
    setDensity: (d: Density) => void;
    detailsOpen: boolean;
    toggleDetails: () => void;
    bandwidth: string;
}) {
    return (
        <header className="topbar">
            <div className="wordmark">
                <Icon name="logo"/>
                <span>s4browser</span>
            </div>
            <div className="vsep"/>
            <ProfileMenu profiles={profiles}/>
            <div className="spacer"/>
            <span className="meta" title="Set the caps in Transfer settings, in the transfers area">{bandwidth}</span>
            <ViewMenu theme={theme} setTheme={setTheme} density={density} setDensity={setDensity}/>
            <button type="button" className="btn ghost icon-only" onClick={toggleDetails}
                    aria-pressed={detailsOpen} aria-label={detailsOpen ? 'Hide details panel' : 'Show details panel'}
                    title={detailsOpen ? 'Hide details panel' : 'Show details panel'}>
                <Icon name="panelRight"/>
            </button>
        </header>
    );
}

// Re-renders once a minute so "expires in" stays current.
function useMinuteTick() {
    const [, set] = useState(0);
    useEffect(() => {
        const t = setInterval(() => set((n) => n + 1), 30_000);
        return () => clearInterval(t);
    }, []);
}

function ProfileMenu({profiles}: {profiles: Profiles}) {
    const {state, active, status} = profiles;
    const [open, setOpen] = useState(false);
    const [dialog, setDialog] = useState<'' | 'add' | 'delete'>('');
    const [error, setError] = useState('');
    const btn = useRef<HTMLButtonElement>(null);
    const menu = useRef<HTMLDivElement>(null);
    useDismiss(open, () => setOpen(false), [btn, menu]);
    useMinuteTick();

    let detail = '';
    let tone = '';
    if (!active) {
        detail = state.profiles.length === 0 ? 'No profiles found' : 'Choose a profile';
    } else if (status.state === 'checking') {
        detail = `${kindLabels[active.kind] ?? active.kind} · loading credentials…`;
    } else if (status.state === 'failed') {
        detail = 'Credentials did not load';
        tone = 't-warn';
    } else if (status.state === 'ok' && status.expires) {
        const left = status.expires - Date.now();
        detail = `${kindLabels[active.kind] ?? active.kind} · ${left > 0 ? 'expires in ' + formatRemaining(left) : 'expired'}`;
        if (left < 15 * 60_000) tone = 't-warn';
    } else {
        detail = kindLabels[active.kind] ?? active.kind;
    }

    const run = async (action: () => Promise<unknown>) => {
        setError('');
        try {
            await action();
        } catch (err) {
            setError(String(err));
        }
    };

    return (
        <div className="anchor">
            <button ref={btn} type="button" className="btn ghost profile-btn" aria-haspopup="menu" aria-expanded={open}
                    onClick={() => setOpen((o) => !o)}>
                <Icon name="key" className={tone}/>
                <span className="name ellipsis">{active?.name ?? 'No profile'}</span>
                <span className={`ellipsis ${tone || 'meta'}`} style={{fontSize: 'var(--fs-body)'}}>{detail}</span>
                <Icon name="chevronDown" size="sm"/>
            </button>
            {open && (
                <div ref={menu} className="menu" role="menu" aria-label="Profiles" style={{top: 38, left: 0, width: 360}}>
                    {state.error && <p className="sidebar-note problem" style={{margin: '6px 8px'}}>{state.error}</p>}
                    {state.warnings?.map((w) => <p key={w} className="meta t-warn" style={{margin: '6px 8px'}}>{w}</p>)}
                    <div className="label menu-heading">Profiles</div>
                    {state.profiles.length === 0 && <p className="meta" style={{padding: '4px 8px'}}>No AWS profiles or saved keys were found.</p>}
                    <div className="menu-scroll">
                    {state.profiles.map((p) => (
                        <button key={p.id} type="button" role="menuitemradio" aria-checked={p.id === state.activeId}
                                className="menu-item" disabled={!p.supported}
                                onClick={() => run(async () => {
                                    await profiles.select(p.id);
                                    setOpen(false);
                                })}>
                            <span style={{width: 16, display: 'flex'}}>{p.id === state.activeId && <Icon name="check" size="sm"/>}</span>
                            <span style={{flex: 1, minWidth: 0}}>
                                <span className="ellipsis" style={{display: 'block'}}>{p.name}</span>
                                <span className="meta" style={{display: 'block'}}>
                                    {kindLabels[p.kind] ?? p.kind}
                                    {p.source === 'app' ? ', saved in this app' : ', from AWS files'}
                                    {p.region ? ` · ${p.region}` : ''}
                                    {p.keyHint ? ` · ${p.keyHint}` : ''}
                                </span>
                                {p.problem && <span className="meta t-warn" style={{display: 'block'}}>{p.problem}</span>}
                            </span>
                        </button>
                    ))}
                    </div>
                    {status.state === 'failed' && (
                        <p className="meta t-warn" role="status" style={{padding: '6px 8px'}}>{status.message}</p>
                    )}
                    {error && <p className="meta t-danger" role="alert" style={{padding: '6px 8px'}}>{error}</p>}
                    <div className="menu-sep"/>
                    <button type="button" role="menuitem" className="menu-item" disabled={!active || status.state === 'checking'}
                            onClick={() => profiles.check(state.activeId)}>
                        <Icon name="refresh"/> Load credentials again
                    </button>
                    <button type="button" role="menuitem" className="menu-item" onClick={() => {
                        setOpen(false);
                        setDialog('add');
                    }}>
                        <Icon name="plus"/> Add access keys…
                    </button>
                    {active?.source === 'app' && (
                        <button type="button" role="menuitem" className="menu-item" onClick={() => {
                            setOpen(false);
                            setDialog('delete');
                        }}>
                            <Icon name="trash"/> Delete {active.name}…
                        </button>
                    )}
                </div>
            )}
            {dialog === 'add' && <AddKeysDialog profiles={profiles} onClose={() => setDialog('')}/>}
            {dialog === 'delete' && active && (
                <Dialog title={`Delete ${active.name}?`} role="alertdialog" onClose={() => setDialog('')}>
                    <p>Its keys are removed from {vaultName}. This cannot be undone. Your AWS files are not touched.</p>
                    {error && <p className="t-danger" role="alert">{error}</p>}
                    <div className="actions">
                        <button type="button" className="btn" data-autofocus onClick={() => setDialog('')}>Cancel</button>
                        <button type="button" className="btn danger" onClick={() => run(async () => {
                            await profiles.remove(active.id);
                            setDialog('');
                        })}>Delete profile</button>
                    </div>
                </Dialog>
            )}
        </div>
    );
}

const emptyKeys = {name: '', accessKey: '', secret: '', region: ''};

function AddKeysDialog({profiles, onClose}: {profiles: Profiles; onClose: () => void}) {
    const [form, setForm] = useState(emptyKeys);
    const [error, setError] = useState('');
    const [busy, setBusy] = useState(false);
    const field = (key: keyof typeof emptyKeys) => ({
        value: form[key],
        onChange: (e: {target: {value: string}}) => setForm({...form, [key]: e.target.value}),
    });

    const submit = async (e: FormEvent) => {
        e.preventDefault();
        setError('');
        setBusy(true);
        try {
            await profiles.addKeys(form.name.trim(), form.accessKey.trim(), form.secret, form.region.trim());
            setForm(emptyKeys);
            onClose();
        } catch (err) {
            setError(String(err));
        } finally {
            setBusy(false);
        }
    };

    return (
        <Dialog title="Add access keys" onClose={onClose}>
            <form onSubmit={submit} autoComplete="off" style={{display: 'flex', flexDirection: 'column', gap: 16}}>
                <p>The keys are stored in {vaultName}. Nothing is written to your AWS files.</p>
                <div className="field">
                    <label htmlFor="k-name">Profile name</label>
                    <input id="k-name" className="input" required data-autofocus spellCheck={false} {...field('name')}/>
                </div>
                <div className="field">
                    <label htmlFor="k-id">Access key ID</label>
                    <input id="k-id" className="input mono" required spellCheck={false} {...field('accessKey')}/>
                </div>
                <div className="field">
                    <label htmlFor="k-secret">Secret access key</label>
                    <input id="k-secret" className="input mono" type="password" required {...field('secret')}/>
                </div>
                <div className="field">
                    <label htmlFor="k-region">Default region <span className="hint">(optional)</span></label>
                    <input id="k-region" className="input" placeholder="us-west-2" spellCheck={false} {...field('region')}/>
                </div>
                {error && <p className="t-danger" role="alert">{error}</p>}
                <div className="actions">
                    <button type="button" className="btn" onClick={onClose}>Cancel</button>
                    <button type="submit" className="btn primary" disabled={busy}>{busy ? 'Saving…' : 'Save keys'}</button>
                </div>
            </form>
        </Dialog>
    );
}

function ViewMenu({theme, setTheme, density, setDensity}: {
    theme: ThemeSetting;
    setTheme: (t: ThemeSetting) => void;
    density: Density;
    setDensity: (d: Density) => void;
}) {
    const [open, setOpen] = useState(false);
    const [licenses, setLicenses] = useState(false);
    const btn = useRef<HTMLButtonElement>(null);
    const menu = useRef<HTMLDivElement>(null);
    useDismiss(open, () => setOpen(false), [btn, menu]);

    const item = (checked: boolean, label: string, onClick: () => void) => (
        <button type="button" role="menuitemradio" aria-checked={checked} className="menu-item" onClick={onClick}>
            <span style={{width: 16, display: 'flex'}}>{checked && <Icon name="check" size="sm"/>}</span>{label}
        </button>
    );

    return (
        <div className="anchor">
            <button ref={btn} type="button" className="btn ghost icon-only" aria-haspopup="menu" aria-expanded={open}
                    aria-label="View settings" title="View settings" onClick={() => setOpen((o) => !o)}>
                <Icon name="sliders"/>
            </button>
            {open && (
                <div ref={menu} className="menu" role="menu" aria-label="View settings" style={{top: 34, right: 0}}>
                    <div className="label menu-heading">Theme</div>
                    {item(theme === 'dark', 'Dark', () => setTheme('dark'))}
                    {item(theme === 'light', 'Light', () => setTheme('light'))}
                    {item(theme === 'system', `Match ${osName}`, () => setTheme('system'))}
                    <div className="menu-sep"/>
                    <div className="label menu-heading">Rows</div>
                    {item(density === 'compact', 'Compact', () => setDensity('compact'))}
                    {item(density === 'comfortable', 'Comfortable', () => setDensity('comfortable'))}
                    <div className="menu-sep"/>
                    <button type="button" role="menuitem" className="menu-item" onClick={() => {
                        setOpen(false);
                        setLicenses(true);
                    }}>
                        <span style={{width: 16, display: 'flex'}}/>Licenses…
                    </button>
                    <div className="menu-sep"/>
                    <p className="meta" style={{padding: '4px 8px 6px'}}>Transfer settings arrive with the queue.</p>
                </div>
            )}
            {licenses && <LicensesDialog onClose={() => setLicenses(false)}/>}
        </div>
    );
}

// s4browser's MIT license and the notices of the third-party software it
// contains, embedded in the binary (licenses.go).
function LicensesDialog({onClose}: {onClose: () => void}) {
    const [text, setText] = useState('');
    useEffect(() => {
        Licenses().then(setText, (err) => setText(String(err)));
    }, []);
    return (
        <Dialog title="Licenses" onClose={onClose} width={720}>
            <pre className="mono" tabIndex={0} aria-label="License texts"
                 style={{maxHeight: '60vh', overflow: 'auto', whiteSpace: 'pre-wrap', margin: 0, userSelect: 'text'}}>
                {text || 'Loading…'}
            </pre>
            <div className="actions">
                <button type="button" className="btn" data-autofocus onClick={onClose}>Close</button>
            </div>
        </Dialog>
    );
}
