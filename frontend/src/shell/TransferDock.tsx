import {useState} from 'react';
import {Icon} from '../ui/Icon';
import {Drop} from '../types';

// The transfer queue area. Transfers are wired in stage 2.4; until then it
// lists what was dropped on the listing, so drag-in can be tried by hand.
export function TransferDock({open, onToggle, height, drops, onClearDrops}: {
    open: boolean;
    onToggle: () => void;
    height: number;
    drops: Drop[];
    onClearDrops: () => void;
}) {
    const [tab, setTab] = useState<'progress' | 'failed' | 'done'>('progress');
    const items = drops.flatMap((d) => d.items.map((item, i) => ({...item, at: d.at, id: `${d.at}-${i}`, logError: d.logError})));

    const tabButton = (id: typeof tab, label: string, count: number, danger = false) => (
        <button type="button" role="tab" className="tab" aria-selected={tab === id} aria-controls="dock-body"
                onClick={() => {
                    setTab(id);
                    if (!open) onToggle();
                }}>
            {label} <span className={`count${danger && count ? ' danger' : ''}`}>{count}</span>
        </button>
    );

    return (
        <section className={`dock${open ? '' : ' collapsed'}`} aria-label="Transfers" style={open ? {height} : undefined}>
            <div className="dock-head">
                <div role="tablist" aria-label="Transfers" style={{display: 'flex', gap: 20}}>
                    {tabButton('progress', 'In progress', 0)}
                    {tabButton('failed', 'Failed', 0, true)}
                    {tabButton('done', 'Done', 0)}
                </div>
                <div className="spacer"/>
                <button type="button" className="btn ghost small" disabled title="Transfer settings arrive in stage 2.4">
                    4 files · 16 parts each · 64 MiB parts
                </button>
                <button type="button" className="btn small" disabled title="Arrives in stage 2.4">
                    <Icon name="pause" size="sm"/>Pause all
                </button>
                <button type="button" className="btn ghost small icon-only" onClick={onToggle}
                        aria-expanded={open} aria-label={open ? 'Collapse transfers' : 'Expand transfers'}
                        title={open ? 'Collapse transfers' : 'Expand transfers'}>
                    <Icon name={open ? 'chevronDown' : 'chevronUp'} size="sm"/>
                </button>
            </div>
            {open && (
                <div id="dock-body" className="dock-body" role="tabpanel">
                    {tab === 'progress' && items.length > 0 ? (
                        <>
                            {items.map((it) => (
                                <div key={it.id} className="qrow">
                                    <Icon name={it.kind === 'folder' ? 'folder' : 'file'} label={it.kind}/>
                                    <div style={{minWidth: 0}}>
                                        <div className="qname ellipsis">{it.path.split(/[\\/]/).pop() || it.path}</div>
                                        <div className="qdetail ellipsis mono selectable">{it.path}{it.absolute ? '' : ' (not an absolute path)'}</div>
                                    </div>
                                    <div className="meta">{it.error ?? (it.logError ? `Log write failed: ${it.logError}` : '')}</div>
                                    <span/>
                                    <span/>
                                    <span><span className={`chip${it.kind === 'missing' || it.kind === 'error' ? ' danger' : ''}`}>Dropped, not uploaded</span></span>
                                    <span/>
                                </div>
                            ))}
                            <div style={{padding: '8px 16px', display: 'flex', alignItems: 'center', gap: 12}}>
                                <span className="meta">Uploading arrives in stage 2.4. Until then, dropped paths are only listed and logged.</span>
                                <button type="button" className="btn ghost small" onClick={onClearDrops}>Clear</button>
                            </div>
                        </>
                    ) : (
                        <div className="empty">
                            <span>{tab === 'progress' ? 'No transfers.' : tab === 'failed' ? 'No failed transfers.' : 'No finished transfers.'}</span>
                            {tab === 'progress' && <span className="meta">Uploads and downloads arrive in stage 2.4. Files dropped on the listing are listed here meanwhile.</span>}
                        </div>
                    )}
                </div>
            )}
        </section>
    );
}
