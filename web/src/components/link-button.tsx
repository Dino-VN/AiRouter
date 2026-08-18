import { Link, type LinkProps } from "react-router-dom"
import type { VariantProps } from "class-variance-authority"

import { buttonVariants } from "@/components/ui/button"
import { cn } from "@/lib/utils"

/**
 * A router link that looks like a button. Using this instead of nesting a
 * <Button> inside a <Link> keeps the markup valid.
 */
function LinkButton({
  className,
  variant,
  size,
  ...props
}: LinkProps & VariantProps<typeof buttonVariants>) {
  return (
    <Link
      data-slot="button"
      className={cn(buttonVariants({ variant, size, className }))}
      {...props}
    />
  )
}

export { LinkButton }
