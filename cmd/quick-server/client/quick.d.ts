// Types for /__quick/quick.js.

export interface User {
  email: string;
}

export declare class QuickError extends Error {
  status: number;
}

export declare const id: {
  /** The signed-in visitor, or null on public and access-code sites. */
  me(): Promise<User | null>;
};

declare const quick: { id: typeof id };
export default quick;
