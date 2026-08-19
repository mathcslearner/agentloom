import { cva, type VariantProps } from "class-variance-authority";
import type { HTMLAttributes } from "react";
import { cn } from "@/lib/utils";

const badgeVariants = cva(
  "inline-flex items-center rounded-md border px-2 py-0.5 text-xs font-medium",
  {
    variants: {
      variant: {
        default: "border-transparent bg-primary text-primary-foreground",
        muted: "border-transparent bg-muted text-muted-foreground",
        outline: "text-foreground",
        running: "border-transparent bg-blue-500/15 text-blue-600 dark:text-blue-400",
        succeeded: "border-transparent bg-green-500/15 text-green-600 dark:text-green-400",
        failed: "border-transparent bg-red-500/15 text-red-600 dark:text-red-400",
        parked: "border-transparent bg-amber-500/15 text-amber-600 dark:text-amber-400",
        cancelled: "border-transparent bg-zinc-500/15 text-zinc-600 dark:text-zinc-400",
      },
    },
    defaultVariants: { variant: "default" },
  },
);

export type BadgeProps = HTMLAttributes<HTMLSpanElement> & VariantProps<typeof badgeVariants>;

export function Badge({ className, variant, ...props }: BadgeProps) {
  return <span className={cn(badgeVariants({ variant }), className)} {...props} />;
}

export { badgeVariants };
