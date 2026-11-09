import { useEffect, useRef } from "react";

/** Whether a dialog is still on the page when a long request returns: one
 * closed meanwhile must not close the one opened since, nor set its state. */
export function useMounted() {
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  return mounted;
}
