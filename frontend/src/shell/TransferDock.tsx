import {CSSProperties, useEffect, useLayoutEffect, useRef, useState} from 'react';
import {useVirtualizer} from '@tanstack/react-virtual';
import {CancelAll, CancelJob, ClearFinished, PauseAll, PauseJob, RemoveJob, ResumeAll, ResumeJob, RetryJob} from '../../bindings/s4browser/internal/app/app';
import {Icon, IconName} from '../ui/Icon';
import {Dialog, useDismiss} from '../ui/overlay';
import {formatEta, formatRate, formatSize, plural} from '../ui/format';
import {JobView, SubmitReport} from '../types';
import {Queue, QueueTab} from './useQueue';
import {SettingsDialog, settingsSummary} from './SettingsDialog';
import {RefusalDialog} from './RefusalDialog';

// The transfer queue: three tabs over the jobs Go reports, a virtualized list
// (a folder can queue 100,000 files), per-job and bulk commands, and the
// outcome of the last submit, including what was refused and why.

export type LastSubmit = {verb: 'upload' | 'download'; report: SubmitReport; at: number};

function readQueueRowHeight(): number {
    const v = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--queue-row-h'));
    return v > 0 ? v : 44;
}

type Tone = '' | 'accent' | 'warn' | 'danger' | 'ok';

// The state chip: a word for every state, so colour is never the only signal.
function stateChip(j: JobView): {label: string; tone: Tone} {
    switch (j.state) {
        case 'queued': return {label: 'Queued', tone: ''};
        case 'running':
            if (j.phase === 'preparing') return {label: 'Preparing', tone: 'accent'};
            if (j.phase === 'finalizing') return {label: 'Finishing', tone: 'accent'};
            return {label: j.direction === 'upload' ? 'Uploading' : 'Downloading', tone: 'accent'};
        case 'pausing': return {label: 'Pausing', tone: ''};
        case 'paused': return {label: 'Paused', tone: ''};
        case 'credential-wait': return {label: 'Waiting for credentials', tone: 'warn'};
        case 'cancelling': return {label: 'Cancelling', tone: ''};
        case 'failing': return {label: 'Failing', tone: 'danger'};
        case 'cancelled': return {label: 'Cancelled', tone: ''};
        case 'failed': return {label: 'Failed', tone: 'danger'};
        case 'completed': return {label: 'Done', tone: 'ok'};
    }
    return {label: j.state, tone: ''};
}

const where = (j: JobView) => `${j.bucket}/${j.key}`;

function JobRow({j, style, run}: {j: JobView; style: CSSProperties; run: (f: () => Promise<unknown>) => void}) {
    const chip = stateChip(j);
    const pct = j.totalBytes > 0 ? Math.min(100, (j.doneBytes / j.totalBytes) * 100) : (j.state === 'completed' ? 100 : 0);
    // The S3 side first: bucket and key tell jobs apart (folder downloads give
    // many jobs the same file name) and are shorter than the local path.
    const route = `${where(j)}   ${j.direction === 'upload' ? 'from' : 'to'} ${j.localPath}`;
    const reasonTone = j.state === 'failed' || j.state === 'failing' ? 't-danger' : 't-warn';
    const action = (name: IconName, label: string, f: () => Promise<unknown>) => (
        <button type="button" className="btn ghost small icon-only" aria-label={`${label} ${j.name}`} title={label}
                onClick={() => run(f)}>
            <Icon name={name} size="sm"/>
        </button>
    );
    const sizeText = j.totalBytes > 0
        ? (j.finished && j.state === 'completed' ? formatSize(j.totalBytes) : `${formatSize(j.doneBytes)} of ${formatSize(j.totalBytes)}`)
        : (j.state === 'completed' ? '0 B' : '');
    return (
        <div className="qrow vrow" role="row" style={style} title={j.detail || undefined}>
            <span role="cell">
                <Icon name={j.direction === 'upload' ? 'arrowUpSmall' : 'arrowDownSmall'} label={j.direction === 'upload' ? 'Upload' : 'Download'}
                      className={j.direction === 'upload' ? 'q-up' : 'q-down'}/>
            </span>
            <div role="cell" style={{minWidth: 0}}>
                <div className="qname ellipsis">{j.name}</div>
                {j.reason
                    ? <div className={`qdetail ellipsis ${reasonTone}`}>{j.reason}</div>
                    : <div className="qdetail ellipsis mono selectable">{route}</div>}
            </div>
            <div role="cell" className="qprogress">
                <div className="bar" aria-hidden="true"><span className={`fill-${chip.tone || 'neutral'}`} style={{width: `${pct}%`}}/></div>
                <div className="meta ellipsis">
                    {sizeText}
                    {j.partsTotal > 1 && !j.finished ? ` · ${j.partsDone}/${j.partsTotal} parts` : ''}
                    {j.retries > 0 ? <span className="t-warn"> · {plural(j.retries, 'retry', 'retries')}</span> : null}
                </div>
            </div>
            <div role="cell" className="right meta">{j.state === 'running' ? formatRate(j.bytesPerSec) : ''}</div>
            <div role="cell" className="right meta">{j.state === 'running' ? formatEta(j.etaSeconds) : ''}</div>
            <div role="cell">
                <span className={`chip${chip.tone ? ' ' + chip.tone : ''}`}>{chip.label}</span>
                {j.warning && <Icon name="alert" size="sm" className="t-warn" label={j.warning}/>}
            </div>
            <div role="cell" className="qactions">
                {j.canPause && action('pause', 'Pause', () => PauseJob(j.id))}
                {j.canResume && action('play', 'Resume', () => ResumeJob(j.id))}
                {j.canRetry && action('refresh', 'Retry', () => RetryJob(j.id))}
                {j.canCancel && action('close', 'Cancel', () => CancelJob(j.id))}
                {j.canRemove && action('close', 'Remove from list', () => RemoveJob(j.id))}
            </div>
        </div>
    );
}

export function TransferDock({open, onToggle, height, queue, busy, last, onDismissLast, notice, onDismissNotice}: {
    open: boolean;
    onToggle: () => void;
    height: number;
    queue: Queue;
    /** What a submit in progress is doing, or ''. */
    busy: string;
    last: LastSubmit | null;
    onDismissLast: () => void;
    notice: string;
    onDismissNotice: () => void;
}) {
    const [tab, setTab] = useState<QueueTab>('progress');
    const [dialog, setDialog] = useState<'' | 'settings' | 'cancel-all' | 'refusals'>('');
    const [menuOpen, setMenuOpen] = useState(false);
    const [cmdError, setCmdError] = useState('');
    const [hidden, setHidden] = useState<string[]>([]);
    const [rowH, setRowH] = useState(readQueueRowHeight);
    const scrollRef = useRef<HTMLDivElement>(null);
    const menuBtn = useRef<HTMLButtonElement>(null);
    const menu = useRef<HTMLDivElement>(null);
    useDismiss(menuOpen, () => setMenuOpen(false), [menuBtn, menu]);

    const {status, lists, totals, settings} = queue;
    const list = lists[tab];

    useLayoutEffect(() => setRowH(readQueueRowHeight()), []);
    const virtual = useVirtualizer({
        count: list.length,
        getScrollElement: () => scrollRef.current,
        estimateSize: () => rowH,
        overscan: 8,
    });

    // A new submit shows the In progress tab, where its jobs are.
    useEffect(() => {
        if (last) setTab('progress');
    }, [last]);

    const run = (f: () => Promise<unknown>) => {
        setCmdError('');
        f().catch((err) => setCmdError(String(err)));
    };

    const tabButton = (id: QueueTab, label: string, count: number, danger = false) => (
        <button type="button" role="tab" className="tab" aria-selected={tab === id} aria-controls="dock-body"
                onClick={() => {
                    setTab(id);
                    if (!open) onToggle();
                }}>
            {label} <span className={`count${danger && count ? ' danger' : ''}`}>{count.toLocaleString('en-US')}</span>
        </button>
    );

    const warnings = (status?.warnings ?? []).filter((w) => !hidden.includes(w));
    const unfinished = lists.progress.length;
    const finishedCount = lists.done.length + lists.failed.length;

    let aggregate = '';
    if (totals.running > 0) {
        aggregate = `${totals.running} running · ${formatRate(totals.bytesPerSec)}`;
        if (totals.bytesPerSec > 0 && totals.leftBytes > 0) aggregate += ` · ${formatEta(totals.leftBytes / totals.bytesPerSec)} left`;
    } else if (totals.paused > 0) {
        aggregate = `${plural(totals.paused, 'transfer')} paused`;
    }

    const lastLine = last ? submitSummary(last) : null;

    return (
        <section className={`dock${open ? '' : ' collapsed'}`} aria-label="Transfers" style={open ? {height} : undefined}>
            <div className="dock-head">
                <div role="tablist" aria-label="Transfers" style={{display: 'flex', gap: 20}}>
                    {tabButton('progress', 'In progress', unfinished)}
                    {tabButton('failed', 'Failed', lists.failed.length, true)}
                    {tabButton('done', 'Done', lists.done.length)}
                </div>
                <div className="spacer"/>
                {aggregate && <span className="meta" role="status">{aggregate}</span>}
                <button type="button" className="btn ghost small" disabled={!settings} onClick={() => setDialog('settings')}
                        title="Transfer settings">
                    <Icon name="sliders" size="sm"/>{settings ? settingsSummary(settings.current) : 'Settings'}
                </button>
                {totals.anyResume && !totals.anyPause ? (
                    <button type="button" className="btn small" onClick={() => run(ResumeAll)}>
                        <Icon name="play" size="sm"/>Resume all
                    </button>
                ) : (
                    <button type="button" className="btn small" disabled={!totals.anyPause} onClick={() => run(PauseAll)}>
                        <Icon name="pause" size="sm"/>Pause all
                    </button>
                )}
                <div className="anchor">
                    <button ref={menuBtn} type="button" className="btn ghost small icon-only" aria-haspopup="menu" aria-expanded={menuOpen}
                            aria-label="More transfer actions" title="More" onClick={() => setMenuOpen((o) => !o)}>
                        <Icon name="more" size="sm"/>
                    </button>
                    {menuOpen && (
                        <div ref={menu} className="menu" role="menu" style={{bottom: 30, right: 0, top: 'auto'}}>
                            <button type="button" role="menuitem" className="menu-item" disabled={!totals.anyResume}
                                    onClick={() => { setMenuOpen(false); run(ResumeAll); }}>
                                <Icon name="play"/> Resume all
                            </button>
                            <button type="button" role="menuitem" className="menu-item" disabled={lists.failed.length === 0}
                                    onClick={() => {
                                        setMenuOpen(false);
                                        run(() => Promise.all(lists.failed.filter((j) => j.canRetry).map((j) => RetryJob(j.id))));
                                    }}>
                                <Icon name="refresh"/> Retry all failed
                            </button>
                            <div className="menu-sep"/>
                            <button type="button" role="menuitem" className="menu-item" disabled={lists.done.length === 0}
                                    onClick={() => { setMenuOpen(false); run(() => ClearFinished(false)); }}>
                                <Icon name="check"/> Clear done and cancelled
                            </button>
                            <button type="button" role="menuitem" className="menu-item" disabled={finishedCount === 0}
                                    onClick={() => { setMenuOpen(false); run(() => ClearFinished(true)); }}>
                                <Icon name="check"/> Clear all finished, failed included
                            </button>
                            <div className="menu-sep"/>
                            <button type="button" role="menuitem" className="menu-item t-danger" disabled={unfinished === 0}
                                    onClick={() => { setMenuOpen(false); setDialog('cancel-all'); }}>
                                <Icon name="close"/> Cancel all…
                            </button>
                        </div>
                    )}
                </div>
                <button type="button" className="btn ghost small icon-only" onClick={onToggle}
                        aria-expanded={open} aria-label={open ? 'Collapse transfers' : 'Expand transfers'}
                        title={open ? 'Collapse transfers' : 'Expand transfers'}>
                    <Icon name={open ? 'chevronDown' : 'chevronUp'} size="sm"/>
                </button>
            </div>
            {open && (
                <>
                    {status && !status.available && (
                        <div className="banner danger" role="alert">
                            <Icon name="alert"/>
                            <div><strong>Transfers are not available</strong>{status.error || queue.error}</div>
                        </div>
                    )}
                    {status?.uploadsDisabled && (
                        <div className="banner warn" role="status">
                            <Icon name="alert"/>
                            <div><strong>Uploads are turned off</strong>{status.uploadsDisabledReason}</div>
                        </div>
                    )}
                    {warnings.map((w) => (
                        <div key={w} className="banner warn" role="status">
                            <Icon name="alert"/>
                            <div style={{flex: 1}}>{w}</div>
                            <button type="button" className="btn ghost small" onClick={() => setHidden((h) => [...h, w])}>Dismiss</button>
                        </div>
                    ))}
                    {notice && (
                        <div className="banner warn" role="status">
                            <Icon name="alert"/>
                            <div style={{flex: 1}}>{notice}</div>
                            <button type="button" className="btn ghost small" onClick={onDismissNotice}>Dismiss</button>
                        </div>
                    )}
                    {cmdError && (
                        <div className="banner danger" role="alert">
                            <Icon name="alert"/>
                            <div style={{flex: 1}}>{cmdError}</div>
                            <button type="button" className="btn ghost small" onClick={() => setCmdError('')}>Dismiss</button>
                        </div>
                    )}
                    {busy && (
                        <div className="dock-line" role="status"><span className="spinner" aria-hidden="true"/>{busy}</div>
                    )}
                    {lastLine && !busy && (
                        <div className={`dock-line${last!.report.refusedTotal ? ' warn' : ''}`} role="status">
                            <Icon name={last!.report.refusedTotal ? 'alert' : 'check'} size="sm"
                                  className={last!.report.refusedTotal ? 't-warn' : 't-ok'}/>
                            <span className="ellipsis" style={{flex: 1}}>{lastLine}</span>
                            {last!.report.refusedTotal > 0 && (
                                <button type="button" className="btn small" onClick={() => setDialog('refusals')}>
                                    Show {last!.report.refusedTotal === 1 ? 'why' : `the ${last!.report.refusedTotal.toLocaleString('en-US')}`}
                                </button>
                            )}
                            <button type="button" className="btn ghost small icon-only" aria-label="Dismiss" title="Dismiss" onClick={onDismissLast}>
                                <Icon name="close" size="sm"/>
                            </button>
                        </div>
                    )}
                    <div id="dock-body" className="dock-body" role="tabpanel" ref={scrollRef}>
                        {list.length > 0 ? (
                            <div role="table" aria-label={tab === 'progress' ? 'Transfers in progress' : tab === 'failed' ? 'Failed transfers' : 'Finished transfers'}
                                 aria-rowcount={list.length} className="qtable" style={{height: virtual.getTotalSize()}}>
                                {virtual.getVirtualItems().map((v) => {
                                    const j = list[v.index];
                                    return <JobRow key={j.id} j={j} run={run}
                                                   style={{transform: `translateY(${v.start}px)`, height: rowH}}/>;
                                })}
                            </div>
                        ) : (
                            <div className="empty">
                                <span>{tab === 'progress' ? 'No transfers.' : tab === 'failed' ? 'No failed transfers.' : 'No finished transfers.'}</span>
                                {tab === 'progress' && <span className="meta">Drop files or folders on the listing, or use Upload. Select objects and use Download.</span>}
                            </div>
                        )}
                    </div>
                </>
            )}
            {dialog === 'settings' && settings && (
                <SettingsDialog state={settings} save={queue.saveSettings} onClose={() => setDialog('')}/>
            )}
            {dialog === 'refusals' && last && (
                <RefusalDialog last={last} onClose={() => setDialog('')}/>
            )}
            {dialog === 'cancel-all' && (
                <Dialog title={`Cancel ${plural(unfinished, 'transfer')}?`} role="alertdialog" onClose={() => setDialog('')}>
                    <p>Unfinished uploads are aborted on S3 and their uploaded parts are discarded. Partial downloads are deleted. Finished transfers are not affected.</p>
                    <div className="actions">
                        <button type="button" className="btn" data-autofocus onClick={() => setDialog('')}>Keep transferring</button>
                        <button type="button" className="btn danger" onClick={() => {
                            setDialog('');
                            run(CancelAll);
                        }}>Cancel transfers</button>
                    </div>
                </Dialog>
            )}
        </section>
    );
}

// One line about the last submit: what was queued, skipped and refused.
export function submitSummary({verb, report}: LastSubmit): string {
    const parts: string[] = [];
    parts.push(report.queued === 0 ? `Nothing was queued to ${verb}` : `Queued ${plural(report.queued, 'file')} to ${verb}`);
    if (report.refusedTotal) parts.push(`${report.refusedTotal.toLocaleString('en-US')} not queued`);
    if (report.emptyFolders) parts.push(`${plural(report.emptyFolders, 'empty folder')} skipped (S3 has no empty folders)`);
    if (report.placeholders) parts.push(`${plural(report.placeholders, 'folder marker')} skipped`);
    return parts.join(' · ');
}
