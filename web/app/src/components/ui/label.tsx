import type { LabelHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

/** A form label, shadcn-style. */
export function Label({ className, ...props }: LabelHTMLAttributes<HTMLLabelElement>) {
  return <label className={cn("text-xs font-medium leading-none text-foreground", className)} {...props} />;
}
