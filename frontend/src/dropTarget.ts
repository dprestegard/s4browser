// Native file drop in Wails v3: the runtime only reports a drop that lands on
// an element carrying DROP_TARGET_ATTR (it walks up from the element under the
// pointer), and while a drag hovers over such an element it adds
// DROP_TARGET_ACTIVE_CLASS to it. Wails v2 used a CSS variable and a different
// class, and neither does anything now: a drop on an element without the
// attribute is silently ignored, so the upload never starts.
// These names are the runtime's; src/dropTarget.test.ts checks them.
export const DROP_TARGET_ATTR = 'data-file-drop-target';
export const DROP_TARGET_ACTIVE_CLASS = 'file-drop-target-active';

// dropTargetProps returns the props that make an element a drop target, or
// none when drops should be ignored there.
export function dropTargetProps(enabled: boolean): Record<string, string> {
    return enabled ? {[DROP_TARGET_ATTR]: ''} : {};
}
