### Fixed
- **Manual import scan with a symlinked library folder** (#2868): files already in your library no longer show up as importable when the library folder is reached through a symlink and the file was offline when Bindery last indexed it or has been edited since. The scan tests now pass on macOS and the macOS lint run is clean again. Thanks magrhino for the report.
