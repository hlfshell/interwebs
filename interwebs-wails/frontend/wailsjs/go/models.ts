export namespace site {
	
	export interface Settings {
	    bufferSites: number;
	    bufferBytes: number;
	    maxSiteBytes: number;
	}
	export interface Record {
	    sequence: number;
	    hash: string;
	    signature: number[];
	}
	export interface Identity {
	    key: string;
	    salt?: string;
	    hash?: string;
	}
	export interface View {
	    id: string;
	    name: string;
	    source?: string;
	    // Go type: Identity
	    identity: any;
	    // Go type: Record
	    record: any;
	    history: Record[];
	    current: string;
	    favorite: boolean;
	    hosting: boolean;
	    live: boolean;
	    // Go type: time
	    lastUsed: any;
	    excluded: string[];
	    status: string;
	    cachedVersions: string[];
	    retained: boolean;
	    magnet: string;
	    url: string;
	    active: boolean;
	    bytes: number;
	    total: number;
	    peers: number;
	    downloadRate: number;
	    uploadRate: number;
	}
	export interface Status {
	    sites: View[];
	    settings: Settings;
	    network: string;
	    paused: boolean;
	}

}

