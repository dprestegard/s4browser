// What a drop does: upload at once when nothing would be replaced, ask when
// something would be, and also ask when that cannot be known (the check failed)
// or uploads are blocked (the dialog says why).
export type DropDecision =
    | {action: 'upload'}
    | {action: 'confirm'; conflicts: string[]; unchecked: boolean};

export async function decideDrop(check: () => Promise<string[]>, blocked: string): Promise<DropDecision> {
    if (blocked) return {action: 'confirm', conflicts: [], unchecked: false};
    try {
        const conflicts = (await check()) ?? [];
        return conflicts.length === 0 ? {action: 'upload'} : {action: 'confirm', conflicts, unchecked: false};
    } catch {
        return {action: 'confirm', conflicts: [], unchecked: true};
    }
}
