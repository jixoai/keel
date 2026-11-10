// The gallery app's entry. nsh resolves an app directory's entry as App.tsx by
// convention, so this thin file exists to mount KitGallery.tsx — the gallery
// itself stays the file a reviewer reads.
import { mount } from "nsh";
import { KitGallery } from "./KitGallery";

mount(KitGallery);
