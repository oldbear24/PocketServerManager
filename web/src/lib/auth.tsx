import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import { currentUser, pb, type Role, type User } from "./pb";

type AuthState = {
  user: User | null;
  signIn: (email: string, password: string) => Promise<void>;
  signOut: () => void;
};

const AuthContext = createContext<AuthState | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(currentUser);

  useEffect(() => pb.authStore.onChange(() => setUser(currentUser())), []);

  const signIn = useCallback(async (email: string, password: string) => {
    await pb.collection("users").authWithPassword(email, password);
  }, []);

  const signOut = useCallback(() => pb.authStore.clear(), []);

  const value = useMemo(() => ({ user, signIn, signOut }), [user, signIn, signOut]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const auth = useContext(AuthContext);
  if (!auth) {
    throw new Error("useAuth needs an AuthProvider");
  }
  return auth;
}

const rank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 };

/** Whether a user has at least a role. The panel enforces this too, on every call. */
export function hasRole(user: User | null, minimum: Role): boolean {
  return Boolean(user) && rank[user!.role] >= rank[minimum];
}
