#!/bin/bash

touch archive.tar.xz
tar -cf - contacts/ application_pages.txt | xz -9e -T0 > archive.tar.xz
