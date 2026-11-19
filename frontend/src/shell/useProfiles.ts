import {useCallback, useEffect, useRef, useState} from 'react';
import {
    AddKeyProfile,
    CheckProfile,
    DeleteProfile,
    Profiles as LoadProfiles,
    SelectProfile,
} from '../../wailsjs/go/main/App';
import {CredentialStatus, ProfileState} from '../types';

// Profile list, the active profile and whether its credentials load. Loading
// credentials runs a credential_process helper if the profile has one; it makes
// no AWS request.
export function useProfiles() {
    const [state, setState] = useState<ProfileState>({profiles: [], activeId: ''});
    const [loaded, setLoaded] = useState(false);
    const [status, setStatus] = useState<CredentialStatus>({state: 'none'});
    const checkSeq = useRef(0);

    const reload = useCallback(async () => {
        const s = (await LoadProfiles()) as ProfileState;
        setState(s);
        setLoaded(true);
        return s;
    }, []);

    useEffect(() => {
        reload().catch((err) => {
            setState({profiles: [], activeId: '', error: String(err)});
            setLoaded(true);
        });
    }, [reload]);

    const check = useCallback(async (id: string) => {
        const seq = ++checkSeq.current;
        if (!id) {
            setStatus({state: 'none'});
            return;
        }
        setStatus({state: 'checking'});
        try {
            const r = await CheckProfile(id);
            if (seq !== checkSeq.current) return;
            setStatus(r.ok
                ? {state: 'ok', keyHint: r.keyHint, expires: r.expires ? new Date(r.expires as unknown as string).getTime() : undefined}
                : {state: 'failed', message: r.message ?? 'The credentials did not load.'});
        } catch (err) {
            if (seq === checkSeq.current) setStatus({state: 'failed', message: String(err)});
        }
    }, []);

    // Check whenever the active profile changes.
    useEffect(() => {
        if (loaded) check(state.activeId);
    }, [loaded, state.activeId, check]);

    const select = useCallback(async (id: string) => {
        await SelectProfile(id);
        await reload();
    }, [reload]);

    const addKeys = useCallback(async (name: string, accessKey: string, secret: string, region: string) => {
        await AddKeyProfile(name, accessKey, secret, region);
        await reload();
    }, [reload]);

    const remove = useCallback(async (id: string) => {
        await DeleteProfile(id);
        await reload();
    }, [reload]);

    const active = state.profiles.find((p) => p.id === state.activeId);
    return {state, loaded, active, status, check, select, addKeys, remove, reload};
}

export type Profiles = ReturnType<typeof useProfiles>;
