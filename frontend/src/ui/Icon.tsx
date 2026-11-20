// The app's icon set: 24-unit stroke icons drawn at 16 px with a 1.6 stroke.
// Decorative by default; pass a label when the icon carries meaning on its own.

const paths = {
    logo: <><rect x="3" y="3" width="12" height="12" rx="2"/><rect x="9" y="9" width="12" height="12" rx="2"/></>,
    folder: <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>,
    folderPlus: <><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/><path d="M12 10v6"/><path d="M9 13h6"/></>,
    file: <><path d="M6 3h8l4 4v14H6z"/><path d="M14 3v4h4"/></>,
    bucket: <><path d="M4 7c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3z"/><path d="M4 7l1.6 11.2c.2 1.6 3 2.8 6.4 2.8s6.2-1.2 6.4-2.8L20 7"/></>,
    upload: <><path d="M12 16V4"/><path d="M7 9l5-5 5 5"/><path d="M4 20h16"/></>,
    download: <><path d="M12 4v12"/><path d="M7 11l5 5 5-5"/><path d="M4 20h16"/></>,
    arrowUpSmall: <><path d="M12 16V4"/><path d="M7 9l5-5 5 5"/></>,
    arrowDownSmall: <><path d="M12 4v12"/><path d="M7 11l5 5 5-5"/></>,
    key: <><circle cx="8" cy="15" r="4"/><path d="M11 12l9-9"/><path d="M17 6l3 3"/></>,
    link: <><path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1"/><path d="M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1"/></>,
    trash: <><path d="M4 7h16"/><path d="M9 7V4h6v3"/><path d="M6 7l1 13h10l1-13"/></>,
    pause: <><path d="M9 5v14"/><path d="M15 5v14"/></>,
    play: <path d="M7 5l12 7-12 7z"/>,
    more: <><circle cx="12" cy="5" r=".8"/><circle cx="12" cy="12" r=".8"/><circle cx="12" cy="19" r=".8"/></>,
    copy: <><rect x="8" y="8" width="12" height="12" rx="2"/><path d="M16 8V5a1 1 0 0 0-1-1H5a1 1 0 0 0-1 1v10a1 1 0 0 0 1 1h3"/></>,
    close: <><path d="M6 6l12 12"/><path d="M18 6L6 18"/></>,
    refresh: <><path d="M20 11a8 8 0 1 0-2.3 5.7"/><path d="M20 4v7h-7"/></>,
    clock: <><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></>,
    check: <path d="M5 12l5 5 9-10"/>,
    alert: <><path d="M12 3l10 18H2z"/><path d="M12 10v4"/><path d="M12 17v.01"/></>,
    plus: <><path d="M12 5v14"/><path d="M5 12h14"/></>,
    back: <><path d="M19 12H5"/><path d="M11 6l-6 6 6 6"/></>,
    up: <><path d="M12 19V5"/><path d="M6 11l6-6 6 6"/></>,
    chevronDown: <path d="M6 9l6 6 6-6"/>,
    chevronUp: <path d="M6 15l6-6 6 6"/>,
    chevronLeft: <path d="M15 6l-6 6 6 6"/>,
    chevronRight: <path d="M9 6l6 6-6 6"/>,
    sliders: <><path d="M4 6h9"/><path d="M17 6h3"/><path d="M4 12h3"/><path d="M11 12h9"/><path d="M4 18h11"/><path d="M19 18h1"/><circle cx="15" cy="6" r="2"/><circle cx="9" cy="12" r="2"/><circle cx="17" cy="18" r="2"/></>,
    panelRight: <><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M15 4v16"/></>,
    panelBottom: <><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M3 15h18"/></>,
} as const;

export type IconName = keyof typeof paths;

export function Icon({name, size, label, className}: {
    name: IconName;
    size?: 'sm' | 'xs' | number;
    label?: string;
    className?: string;
}) {
    const cls = ['icon', typeof size === 'string' ? size : '', className ?? ''].filter(Boolean).join(' ');
    const style = typeof size === 'number' ? {width: size, height: size} : undefined;
    return (
        <svg className={cls} style={style} viewBox="0 0 24 24"
             {...(label ? {role: 'img', 'aria-label': label} : {'aria-hidden': true})}>
            {paths[name]}
        </svg>
    );
}
