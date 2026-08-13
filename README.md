# RowBot

A Discord bot that posts your Concept2 Logbook results (rowing, skiing, biking, and more) to your Discord server as a generated result card, whenever you finish a workout. Built with Go, HTMX, and Tailwind/DaisyUI.

## TODO

- Update the build/test/release workflow to be more reasonable
- See if more info can be added to the Discord bot automatically, like release version
- `/setchannel` silently replaces a guild's previously configured reporting channel (one channel per guild, enforced by a unique constraint) with no warning to the user that this is happening — document the behavior and/or have the command warn when it's overwriting an existing channel rather than setting one for the first time. or allow a user to configure multiple channels
